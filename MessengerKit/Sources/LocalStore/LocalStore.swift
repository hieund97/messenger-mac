import CoreBridge
import Foundation
import GRDB

/// Kết quả một lần ghi tin: tin sau khi ghi và các dòng bị thay (tin optimistic).
public struct MessageChange: Sendable, Equatable {
    public var message: Message
    public var removedIDs: [String]

    public init(message: Message, removedIDs: [String] = []) {
        self.message = message
        self.removedIDs = removedIDs
    }
}

/// Kho tin nhắn chỉ nằm trong bộ nhớ (DatabaseQueue in-memory): tin nhắn không bao giờ được ghi ra đĩa.
/// Vì không có dữ liệu sống qua lần khởi động nên không cần xử lý tin `sending` còn sót khi mở lại.
/// Mọi API đọc/ghi là async, chạy ngoài main actor.
public final class LocalStore: Sendable {
    /// id của tin optimistic chưa có id thật.
    public static let optimisticPrefix = "local-"
    private let writer: any DatabaseWriter

    public init() throws {
        writer = try DatabaseQueue()
        try Schema.migrator().migrate(writer)
    }

    public func close() throws { try writer.close() }

    // MARK: Conversation

    /// Ghi hội thoại. Nếu `last_activity_ts` không mới hơn mốc đã đọc cục bộ thì giữ `unread_count` hiện có.
    @discardableResult
    public func upsert(_ c: Conversation) async throws -> Conversation {
        try await writer.write { db in
            try db.execute(sql: """
                INSERT INTO conversation(id, peer_id, name, avatar_url, last_message_preview, last_activity_ts, unread_count, history_restored)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(id) DO UPDATE SET
                    peer_id = excluded.peer_id, name = excluded.name, avatar_url = excluded.avatar_url,
                    last_message_preview = excluded.last_message_preview, last_activity_ts = excluded.last_activity_ts,
                    unread_count = CASE WHEN conversation.local_read_ts > 0 AND excluded.last_activity_ts <= conversation.local_read_ts
                                        THEN conversation.unread_count ELSE excluded.unread_count END,
                    history_restored = excluded.history_restored
                """, arguments: [c.id, c.peerID, c.name, c.avatarURL, c.lastMessagePreview, c.lastActivityTS, c.unreadCount, c.historyRestored])
            return try Self.conversation(id: c.id, db)!
        }
    }

    public func conversation(id: String) async throws -> Conversation? {
        try await writer.read { db in try Self.conversation(id: id, db) }
    }

    /// Mới nhất trước.
    public func conversations() async throws -> [Conversation] {
        try await writer.read { db in
            try Row.fetchAll(db, sql: "SELECT * FROM conversation ORDER BY last_activity_ts DESC, id").map(Self.conversation)
        }
    }

    /// Đánh dấu đã đọc tới `untilTS`: đặt mốc đọc cục bộ và đếm lại `unread_count` từ DB
    /// (tin không phải của mình có `ts` > `untilTS`).
    @discardableResult
    public func markRead(conversationID: String, untilTS: Int64) async throws -> Conversation? {
        try await writer.write { db in
            try db.execute(sql: """
                UPDATE conversation SET
                    local_read_ts = MAX(local_read_ts, ?),
                    unread_count = (SELECT COUNT(*) FROM message WHERE conversation_id = ? AND is_outgoing = 0 AND ts > ?)
                WHERE id = ?
                """, arguments: [untilTS, conversationID, untilTS, conversationID])
            return try Self.conversation(id: conversationID, db)
        }
    }

    // MARK: Message

    /// Thêm hoặc cập nhật tin:
    /// - trùng `id` → cập nhật dòng đó;
    /// - chưa có `id` nhưng `client_id` trùng một tin optimistic → thay dòng optimistic bằng tin này (id thật);
    /// - còn lại → thêm mới.
    public func upsert(_ m: Message) async throws -> MessageChange {
        try await writer.write { db in
            let removed = try Self.upsert(m, db)
            return MessageChange(message: try Self.fetch(id: m.id, db)!, removedIDs: removed)
        }
    }

    /// `limit` tin cuối của hội thoại, cũ trước, mới sau.
    public func messages(conversationID: String, limit: Int) async throws -> [Message] {
        try await writer.read { db in
            let rows = try Row.fetchAll(db, sql: """
                SELECT * FROM (SELECT * FROM message WHERE conversation_id = ? ORDER BY ts DESC, id DESC LIMIT ?)
                ORDER BY ts, id
                """, arguments: [conversationID, limit])
            return try Self.messages(rows, db)
        }
    }

    public func message(clientID: String) async throws -> Message? {
        try await writer.read { db in
            guard let id = try String.fetchOne(db, sql: "SELECT id FROM message WHERE client_id = ?", arguments: [clientID]) else { return nil }
            return try Self.fetch(id: id, db)
        }
    }

    /// Cập nhật trạng thái tin theo `client_id`; trả về `nil` nếu không có tin nào mang `client_id` đó.
    /// - Không hạ trạng thái: tin đã `.sent` hoặc đã có id thật thì `sending/failed/unknown` bị bỏ qua.
    /// - `.sent` kèm `messageID`: nếu dòng id đó đã có (echo đến trước) thì xoá dòng optimistic và gán
    ///   `client_id` + `sent` cho dòng server, giữ nguyên ts/text/attachments của server; chưa có thì đổi id dòng optimistic.
    public func setStatus(clientID: String, status: MessageStatus, messageID: String? = nil) async throws -> MessageChange? {
        try await writer.write { db in
            guard let row = try Row.fetchOne(db, sql: "SELECT id, status FROM message WHERE client_id = ?", arguments: [clientID]) else { return nil }
            let id: String = row["id"]
            let current = MessageStatus(rawValue: row["status"]) ?? .sent

            guard status == .sent else {
                guard id.hasPrefix(Self.optimisticPrefix), current != .sent else {
                    return MessageChange(message: try Self.fetch(id: id, db)!, removedIDs: [])
                }
                try db.execute(sql: "UPDATE message SET status = ? WHERE id = ?", arguments: [status.rawValue, id])
                return MessageChange(message: try Self.fetch(id: id, db)!, removedIDs: [])
            }

            guard let mid = messageID, mid != id else {
                try db.execute(sql: "UPDATE message SET status = 'sent' WHERE id = ?", arguments: [id])
                return MessageChange(message: try Self.fetch(id: id, db)!, removedIDs: [])
            }
            if try Self.fetch(id: mid, db) != nil {
                try db.execute(sql: "DELETE FROM message WHERE id = ?", arguments: [id])
                try db.execute(sql: "UPDATE message SET client_id = ?, status = 'sent' WHERE id = ?", arguments: [clientID, mid])
                return MessageChange(message: try Self.fetch(id: mid, db)!, removedIDs: [id])
            }
            var m = try Self.fetch(id: id, db)!
            m.id = mid
            m.status = .sent
            let removed = try Self.upsert(m, db)
            return MessageChange(message: try Self.fetch(id: mid, db)!, removedIDs: removed)
        }
    }

    // MARK: Nội bộ

    /// Trả về id các dòng bị xoá (tin optimistic trùng `client_id`).
    private static func upsert(_ m: Message, _ db: Database) throws -> [String] {
        var removed: [String] = []
        if let cid = m.clientID,
           let other = try String.fetchOne(db, sql: "SELECT id FROM message WHERE client_id = ?", arguments: [cid]),
           other != m.id {
            try db.execute(sql: "DELETE FROM message WHERE id = ?", arguments: [other])
            removed.append(other)
        }
        try db.execute(sql: """
            INSERT INTO message(id, conversation_id, thread_id, e2ee, sender_id, is_outgoing, ts, text, client_id, status, reply_to_id)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(id) DO UPDATE SET
                conversation_id = excluded.conversation_id, thread_id = excluded.thread_id, e2ee = excluded.e2ee,
                sender_id = excluded.sender_id, is_outgoing = excluded.is_outgoing, ts = excluded.ts, text = excluded.text,
                client_id = COALESCE(excluded.client_id, message.client_id), status = excluded.status,
                reply_to_id = excluded.reply_to_id
            """, arguments: [m.id, m.conversationID, m.threadID, m.e2ee, m.senderID, m.isOutgoing, m.ts, m.text,
                             m.clientID, m.status.rawValue, m.replyToID])
        try db.execute(sql: "DELETE FROM attachment WHERE message_id = ?", arguments: [m.id])
        for (i, a) in m.attachments.enumerated() {
            try db.execute(sql: """
                INSERT INTO attachment(message_id, position, ref, kind, mime, filename, size, width, height, preview_url)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """, arguments: [m.id, i, a.ref, a.kind, a.mime, a.filename, a.size, a.width, a.height, a.previewURL])
        }
        return removed
    }

    private static func conversation(id: String, _ db: Database) throws -> Conversation? {
        try Row.fetchOne(db, sql: "SELECT * FROM conversation WHERE id = ?", arguments: [id]).map(conversation)
    }

    private static func conversation(_ r: Row) -> Conversation {
        Conversation(id: r["id"], peerID: r["peer_id"], name: r["name"], avatarURL: r["avatar_url"],
                     lastMessagePreview: r["last_message_preview"], lastActivityTS: r["last_activity_ts"],
                     unreadCount: r["unread_count"], historyRestored: r["history_restored"])
    }

    private static func fetch(id: String, _ db: Database) throws -> Message? {
        let rows = try Row.fetchAll(db, sql: "SELECT * FROM message WHERE id = ?", arguments: [id])
        return try messages(rows, db).first
    }

    /// Một query IN cho attachment của cả loạt tin.
    private static func messages(_ rows: [Row], _ db: Database) throws -> [Message] {
        guard !rows.isEmpty else { return [] }
        let ids = rows.map { $0["id"] as String }
        let attRows = try Row.fetchAll(db, sql: """
            SELECT * FROM attachment WHERE message_id IN (\(databaseQuestionMarks(count: ids.count))) ORDER BY message_id, position
            """, arguments: StatementArguments(ids))
        var byMessage: [String: [Attachment]] = [:]
        for a in attRows {
            byMessage[a["message_id"], default: []].append(
                Attachment(ref: a["ref"], kind: a["kind"], mime: a["mime"], filename: a["filename"], size: a["size"],
                           width: a["width"], height: a["height"], previewURL: a["preview_url"]))
        }
        return rows.map { r in
            let id: String = r["id"]
            return Message(id: id, conversationID: r["conversation_id"], threadID: r["thread_id"], e2ee: r["e2ee"],
                           senderID: r["sender_id"], isOutgoing: r["is_outgoing"], ts: r["ts"], text: r["text"],
                           clientID: r["client_id"], attachments: byMessage[id] ?? [], replyToID: r["reply_to_id"],
                           status: MessageStatus(rawValue: r["status"]) ?? .sent)
        }
    }
}

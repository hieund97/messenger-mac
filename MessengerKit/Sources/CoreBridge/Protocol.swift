import Foundation
import os

let bridgeLogger = Logger(subsystem: "dev.hieund.macmess", category: "core-bridge")

/// JSON thô dùng cho args/data của giao thức.
public enum JSONValue: Codable, Sendable, Equatable {
    case null
    case bool(Bool)
    case int(Int64)
    case double(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Int64.self) { self = .int(v) }
        else if let v = try? c.decode(Double.self) { self = .double(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let v): try c.encode(v)
        case .int(let v): try c.encode(v)
        case .double(let v): try c.encode(v)
        case .string(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        }
    }
}

public struct CoreConfig: Codable, Sendable, Equatable {
    public var dataDir: String
    public var backend: String
    public var logLevel: String

    public init(dataDir: String = "", backend: String, logLevel: String = "info") {
        self.dataDir = dataDir
        self.backend = backend
        self.logLevel = logLevel
    }

    enum CodingKeys: String, CodingKey {
        case dataDir = "data_dir"
        case backend
        case logLevel = "log_level"
    }
}

public struct CookieParam: Codable, Sendable, Equatable {
    public var name: String
    public var value: String
    public var domain: String
    public var expires: Int64?

    public init(name: String, value: String, domain: String, expires: Int64? = nil) {
        self.name = name
        self.value = value
        self.domain = domain
        self.expires = expires
    }

    enum CodingKeys: String, CodingKey { case name, value, domain, expires }
}

public struct CoreError: Codable, Sendable, Equatable {
    public var code: String
    public var message: String

    public init(code: String, message: String) {
        self.code = code
        self.message = message
    }

    enum CodingKeys: String, CodingKey { case code, message }
}

public struct CoreResponse: Codable, Sendable, Equatable {
    public var ok: Bool
    public var error: CoreError?
    public var data: JSONValue?

    public init(ok: Bool, error: CoreError? = nil, data: JSONValue? = nil) {
        self.ok = ok
        self.error = error
        self.data = data
    }

    enum CodingKeys: String, CodingKey { case ok, error, data }
}

public struct ConnectionState: Codable, Sendable, Equatable {
    public var state: String
    public var reason: String?

    public init(state: String, reason: String? = nil) {
        self.state = state
        self.reason = reason
    }

    enum CodingKeys: String, CodingKey { case state, reason }
}

public struct HistoryRestoreState: Codable, Sendable, Equatable {
    public var state: String
    public var reason: String?

    public init(state: String, reason: String? = nil) {
        self.state = state
        self.reason = reason
    }

    enum CodingKeys: String, CodingKey { case state, reason }
}

public struct MessageSendResult: Codable, Sendable, Equatable {
    public var clientID: String
    public var status: String
    public var messageID: String?
    public var error: CoreError?

    public init(clientID: String, status: String, messageID: String? = nil, error: CoreError? = nil) {
        self.clientID = clientID
        self.status = status
        self.messageID = messageID
        self.error = error
    }

    enum CodingKeys: String, CodingKey {
        case clientID = "client_id"
        case status
        case messageID = "message_id"
        case error
    }
}

public struct ReadReceipt: Codable, Sendable, Equatable {
    public var conversationID: String
    public var readerID: String
    public var untilTS: Int64

    enum CodingKeys: String, CodingKey {
        case conversationID = "conversation_id"
        case readerID = "reader_id"
        case untilTS = "until_ts"
    }
}

public struct Conversation: Codable, Sendable, Equatable {
    public var id: String
    public var peerID: String
    public var name: String
    public var avatarURL: String?
    public var lastMessagePreview: String?
    public var lastActivityTS: Int64
    public var unreadCount: Int
    public var historyRestored: Bool

    public init(id: String, peerID: String, name: String, avatarURL: String? = nil, lastMessagePreview: String? = nil,
                lastActivityTS: Int64 = 0, unreadCount: Int = 0, historyRestored: Bool = true) {
        self.id = id
        self.peerID = peerID
        self.name = name
        self.avatarURL = avatarURL
        self.lastMessagePreview = lastMessagePreview
        self.lastActivityTS = lastActivityTS
        self.unreadCount = unreadCount
        self.historyRestored = historyRestored
    }

    enum CodingKeys: String, CodingKey {
        case id
        case peerID = "peer_id"
        case name
        case avatarURL = "avatar_url"
        case lastMessagePreview = "last_message_preview"
        case lastActivityTS = "last_activity_ts"
        case unreadCount = "unread_count"
        case historyRestored = "history_restored"
    }
}

public struct Attachment: Codable, Sendable, Equatable {
    public var ref: String
    public var kind: String
    public var mime: String
    public var filename: String?
    public var size: Int64?
    public var width: Int?
    public var height: Int?
    public var previewURL: String?

    public init(ref: String, kind: String, mime: String, filename: String? = nil, size: Int64? = nil,
                width: Int? = nil, height: Int? = nil, previewURL: String? = nil) {
        self.ref = ref
        self.kind = kind
        self.mime = mime
        self.filename = filename
        self.size = size
        self.width = width
        self.height = height
        self.previewURL = previewURL
    }

    enum CodingKeys: String, CodingKey {
        case ref, kind, mime, filename, size, width, height
        case previewURL = "preview_url"
    }
}

/// Trạng thái gửi, chỉ có ở phía app (không nằm trong JSON của lõi). Tin nhận từ lõi mặc định `.sent`.
public enum MessageStatus: String, Sendable, Equatable {
    case sending, sent, failed, unknown
}

public struct Message: Codable, Sendable, Equatable {
    public var id: String
    public var conversationID: String
    public var threadID: String
    public var e2ee: Bool
    public var senderID: String
    public var isOutgoing: Bool
    public var ts: Int64
    public var text: String?
    public var clientID: String?
    public var attachments: [Attachment]
    public var replyToID: String?
    /// Không mã hóa/giải mã: lõi không biết trạng thái gửi của app.
    public var status: MessageStatus = .sent

    public init(id: String, conversationID: String, threadID: String, e2ee: Bool = false, senderID: String,
                isOutgoing: Bool, ts: Int64, text: String? = nil, clientID: String? = nil,
                attachments: [Attachment] = [], replyToID: String? = nil, status: MessageStatus = .sent) {
        self.id = id
        self.conversationID = conversationID
        self.threadID = threadID
        self.e2ee = e2ee
        self.senderID = senderID
        self.isOutgoing = isOutgoing
        self.ts = ts
        self.text = text
        self.clientID = clientID
        self.attachments = attachments
        self.replyToID = replyToID
        self.status = status
    }

    enum CodingKeys: String, CodingKey {
        case id
        case conversationID = "conversation_id"
        case threadID = "thread_id"
        case e2ee
        case senderID = "sender_id"
        case isOutgoing = "is_outgoing"
        case ts, text
        case clientID = "client_id"
        case attachments
        case replyToID = "reply_to_id"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        conversationID = try c.decode(String.self, forKey: .conversationID)
        threadID = try c.decode(String.self, forKey: .threadID)
        e2ee = try c.decode(Bool.self, forKey: .e2ee)
        senderID = try c.decode(String.self, forKey: .senderID)
        isOutgoing = try c.decode(Bool.self, forKey: .isOutgoing)
        ts = try c.decode(Int64.self, forKey: .ts)
        text = try c.decodeIfPresent(String.self, forKey: .text)
        clientID = try c.decodeIfPresent(String.self, forKey: .clientID)
        attachments = try c.decodeIfPresent([Attachment].self, forKey: .attachments) ?? []
        replyToID = try c.decodeIfPresent(String.self, forKey: .replyToID)
    }
}

struct CookiesUpdatedPayload: Decodable {
    var cookies: [CookieParam]

    enum CodingKeys: String, CodingKey { case cookies }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        cookies = try c.decodeIfPresent([CookieParam].self, forKey: .cookies) ?? []
    }
}

/// Lệnh gửi sang core. Tên cmd và field args khớp `core/engine/protocol.go`.
public enum CoreCommand: Sendable, Equatable {
    case login(cookies: [CookieParam], platform: String)
    case restoreHistory(pin: String)
    case skipHistoryRestore
    case logout
    case reconnect
    case listConversations(cursor: String?)
    case fetchMessages(conversationID: String, beforeTS: Int64?, limit: Int)
    case sendText(conversationID: String, text: String, clientID: String)
    case sendMedia(conversationID: String, filePath: String, mime: String, clientID: String)
    case markRead(conversationID: String, untilTS: Int64)
    case downloadMedia(attachmentRef: String, destPath: String)

    var name: String {
        switch self {
        case .login: "login"
        case .restoreHistory: "restore_history"
        case .skipHistoryRestore: "skip_history_restore"
        case .logout: "logout"
        case .reconnect: "reconnect"
        case .listConversations: "list_conversations"
        case .fetchMessages: "fetch_messages"
        case .sendText: "send_text"
        case .sendMedia: "send_media"
        case .markRead: "mark_read"
        case .downloadMedia: "download_media"
        }
    }

    var args: [String: JSONValue] {
        switch self {
        case .login(let cookies, let platform):
            return [
                "cookies": .array(cookies.map { c in
                    var o: [String: JSONValue] = ["name": .string(c.name), "value": .string(c.value), "domain": .string(c.domain)]
                    if let e = c.expires { o["expires"] = .int(e) }
                    return .object(o)
                }),
                "platform": .string(platform),
            ]
        case .restoreHistory(let pin):
            return ["pin": .string(pin)]
        case .skipHistoryRestore, .logout, .reconnect:
            return [:]
        case .listConversations(let cursor):
            return cursor.map { ["cursor": .string($0)] } ?? [:]
        case .fetchMessages(let id, let before, let limit):
            var o: [String: JSONValue] = ["conversation_id": .string(id), "limit": .int(Int64(limit))]
            if let before { o["before_ts"] = .int(before) }
            return o
        case .sendText(let id, let text, let clientID):
            return ["conversation_id": .string(id), "text": .string(text), "client_id": .string(clientID)]
        case .sendMedia(let id, let path, let mime, let clientID):
            return ["conversation_id": .string(id), "file_path": .string(path), "mime": .string(mime), "client_id": .string(clientID)]
        case .markRead(let id, let until):
            return ["conversation_id": .string(id), "until_ts": .int(until)]
        case .downloadMedia(let ref, let dest):
            return ["attachment_ref": .string(ref), "dest_path": .string(dest)]
        }
    }
}

/// Sự kiện từ core. Type lạ → `.unknown`, không ném lỗi.
public enum CoreEvent: Decodable, Sendable, Equatable {
    case response(requestID: Int64, CoreResponse)
    case connectionState(ConnectionState)
    case historyRestoreState(HistoryRestoreState)
    case conversationUpsert(Conversation)
    case messageUpsert(Message)
    case messageSendResult(MessageSendResult)
    case readReceipt(ReadReceipt)
    case cookiesUpdated([CookieParam])
    case error(CoreError)
    case unknown(String)

    enum CodingKeys: String, CodingKey {
        case type
        case requestID = "request_id"
        case data
    }

    static func parse(_ json: String) throws -> CoreEvent {
        try JSONDecoder().decode(CoreEvent.self, from: Data(json.utf8))
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let type = try c.decode(String.self, forKey: .type)
        // Chỉ log type + lỗi, không log nội dung (có thể chứa cookie/tin nhắn).
        func payload<T: Decodable>(_ t: T.Type) -> T? {
            do { return try c.decode(T.self, forKey: .data) } catch {
                bridgeLogger.error("decode failed type=\(type, privacy: .public) error=\(String(describing: error), privacy: .public)")
                return nil
            }
        }
        switch type {
        case "response":
            guard let id = try c.decodeIfPresent(Int64.self, forKey: .requestID) else {
                bridgeLogger.error("response without request_id")
                self = .unknown(type)
                return
            }
            // Luôn trả response để không treo continuation.
            self = .response(requestID: id, payload(CoreResponse.self)
                ?? CoreResponse(ok: false, error: CoreError(code: "decode_failed", message: "cannot decode response")))
        case "connection_state":
            self = payload(ConnectionState.self).map(CoreEvent.connectionState) ?? .unknown(type)
        case "history_restore_state":
            self = payload(HistoryRestoreState.self).map(CoreEvent.historyRestoreState) ?? .unknown(type)
        case "conversation_upsert":
            self = payload(Conversation.self).map(CoreEvent.conversationUpsert) ?? .unknown(type)
        case "message_upsert":
            self = payload(Message.self).map(CoreEvent.messageUpsert) ?? .unknown(type)
        case "message_send_result":
            self = payload(MessageSendResult.self).map(CoreEvent.messageSendResult) ?? .unknown(type)
        case "read_receipt":
            self = payload(ReadReceipt.self).map(CoreEvent.readReceipt) ?? .unknown(type)
        case "cookies_updated":
            self = payload(CookiesUpdatedPayload.self).map { CoreEvent.cookiesUpdated($0.cookies) } ?? .unknown(type)
        case "error":
            self = payload(CoreError.self).map(CoreEvent.error) ?? .unknown(type)
        default:
            self = .unknown(type)
        }
    }
}

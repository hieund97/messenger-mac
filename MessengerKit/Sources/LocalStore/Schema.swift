import GRDB

enum Schema {
    static func migrator() -> DatabaseMigrator {
        var migrator = DatabaseMigrator()
        migrator.registerMigration("v1") { db in
            try db.execute(sql: """
                CREATE TABLE conversation(
                    id TEXT PRIMARY KEY NOT NULL,
                    peer_id TEXT NOT NULL,
                    name TEXT NOT NULL,
                    avatar_url TEXT,
                    last_message_preview TEXT,
                    last_activity_ts INTEGER NOT NULL DEFAULT 0,
                    unread_count INTEGER NOT NULL DEFAULT 0,
                    history_restored INTEGER NOT NULL DEFAULT 0
                )
                """)
            try db.execute(sql: """
                CREATE TABLE message(
                    id TEXT PRIMARY KEY NOT NULL,
                    conversation_id TEXT NOT NULL,
                    thread_id TEXT NOT NULL,
                    e2ee INTEGER NOT NULL DEFAULT 0,
                    sender_id TEXT NOT NULL,
                    is_outgoing INTEGER NOT NULL DEFAULT 0,
                    ts INTEGER NOT NULL,
                    text TEXT,
                    client_id TEXT UNIQUE,
                    status TEXT NOT NULL DEFAULT 'sent',
                    reply_to_id TEXT
                )
                """)
            try db.execute(sql: "CREATE INDEX message_conversation_ts ON message(conversation_id, ts)")
            try db.execute(sql: """
                CREATE TABLE attachment(
                    message_id TEXT NOT NULL REFERENCES message(id) ON DELETE CASCADE,
                    position INTEGER NOT NULL,
                    ref TEXT NOT NULL,
                    kind TEXT NOT NULL,
                    mime TEXT NOT NULL,
                    filename TEXT,
                    size INTEGER,
                    width INTEGER,
                    height INTEGER,
                    preview_url TEXT,
                    PRIMARY KEY(message_id, position)
                )
                """)
        }
        // Mốc đã đọc cục bộ: conversation_upsert cũ hơn mốc này không được ghi đè unread_count.
        migrator.registerMigration("v2") { db in
            try db.execute(sql: "ALTER TABLE conversation ADD COLUMN local_read_ts INTEGER NOT NULL DEFAULT 0")
        }
        return migrator
    }
}

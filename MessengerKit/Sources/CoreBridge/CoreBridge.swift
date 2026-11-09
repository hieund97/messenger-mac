@_exported import MessengerCore
import Foundation

public enum CoreBridgeError: Error, Sendable, Equatable {
    case invalidConfig        // core_init = -1
    case unsupportedBackend   // -2
    case alreadyInitialized   // -3 hoặc đã có bridge đang chạy
    case shuttingDown         // -4
    case unknownInitCode(Int32)
    case notRunning           // core_call = -1, hoặc bridge đã shutdown
    case encodingFailed
}

private final class Locked<T: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: T
    init(_ value: T) { self.value = value }
    func withLock<R>(_ body: (inout T) -> R) -> R {
        lock.lock()
        defer { lock.unlock() }
        return body(&value)
    }
}

/// Mốc huỷ của một call. Chỉ đọc/ghi khi đang giữ lock của Hub.
private final class CallToken: @unchecked Sendable {
    var id: Int64?
    var cancelled = false
}

/// Trạng thái dùng chung giữa actor và C callback. Mọi truy cập qua một lock.
private final class Hub: Sendable {
    typealias Continuation = CheckedContinuation<CoreResponse, Error>

    private struct State {
        var pending: [Int64: Continuation] = [:]
        var closed = false
        var shutdownTask: Task<Void, Never>?
    }

    private let state = Locked(State())
    let eventContinuation: AsyncStream<CoreEvent>.Continuation

    init(_ c: AsyncStream<CoreEvent>.Continuation) { eventContinuation = c }

    func call(_ json: String) async throws -> CoreResponse {
        let token = CallToken()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: Continuation) in
                register(json, cont, token)
            }
        } onCancel: {
            cancel(token)
        }
    }

    private func register(_ json: String, _ cont: Continuation, _ token: CallToken) {
        // Giữ lock qua core_call: callback (tra pending dưới cùng lock) luôn thấy pending đã đăng ký.
        let failure: Error? = state.withLock { s in
            if token.cancelled { return CancellationError() }
            if s.closed { return CoreBridgeError.notRunning }
            let id = core_call(json)
            if id < 0 { return CoreBridgeError.notRunning }
            token.id = id
            s.pending[id] = cont
            return nil
        }
        if let failure { cont.resume(throwing: failure) }
    }

    private func cancel(_ token: CallToken) {
        let cont: Continuation? = state.withLock { s in
            token.cancelled = true
            guard let id = token.id else { return nil }
            return s.pending.removeValue(forKey: id)
        }
        cont?.resume(throwing: CancellationError())
    }

    func receive(_ event: CoreEvent) {
        if case .response(let id, let resp) = event {
            let cont: Continuation? = state.withLock { $0.pending.removeValue(forKey: id) }
            cont?.resume(returning: resp)
        } else {
            eventContinuation.yield(event)
        }
    }

    /// Callback không giải mã được cả event: nếu là response thì vẫn phải fail continuation.
    func failResponse(id: Int64) {
        receive(.response(requestID: id, CoreResponse(ok: false, error: CoreError(code: "decode_failed", message: "cannot decode event"))))
    }

    /// Idempotent: mọi lần gọi await cùng một task.
    func shutdown() async {
        let task: Task<Void, Never> = state.withLock { s in
            if let t = s.shutdownTask { return t }
            let t = Task { [self] in
                if currentHub.withLock({ $0 === self }) {
                    // core_shutdown có thể chờ tới 5s: chạy ngoài thread gọi.
                    await withCheckedContinuation { (done: CheckedContinuation<Void, Never>) in
                        DispatchQueue.global().async { core_shutdown(); done.resume() }
                    }
                }
                currentHub.withLock { if $0 === self { $0 = nil } }
                close()
            }
            s.shutdownTask = t
            return t
        }
        await task.value
    }

    private func close() {
        let pending: [Continuation] = state.withLock { s in
            s.closed = true
            let p = Array(s.pending.values)
            s.pending = [:]
            return p
        }
        for c in pending { c.resume(throwing: CoreBridgeError.notRunning) }
        eventContinuation.finish()
    }
}

/// C callback không mang context nên giữ hub hiện tại trong registry tĩnh.
private let currentHub = Locked<Hub?>(nil)

private struct EventEnvelope: Decodable {
    var type: String?
    var requestID: Int64?
    enum CodingKeys: String, CodingKey { case type, requestID = "request_id" }
}

private func coreEventCallback(_ raw: UnsafePointer<CChar>?) {
    guard let raw else { return }
    let json = String(cString: raw) // copy ngay; Go tự free
    guard let hub = currentHub.withLock({ $0 }) else { return }
    do {
        hub.receive(try CoreEvent.parse(json))
    } catch {
        bridgeLogger.error("decode failed (envelope) error=\(String(describing: error), privacy: .public)")
        if let env = try? JSONDecoder().decode(EventEnvelope.self, from: Data(json.utf8)),
           let id = env.requestID, env.type == nil || env.type == "response" {
            hub.failResponse(id: id)
        }
    }
}

/// Cầu nối tới core Go. Core là singleton toàn tiến trình: chỉ một bridge sống tại một thời điểm.
///
/// - Bắt buộc gọi `shutdown()` khi xong (kể cả trong test), nếu không bridge kế tiếp sẽ nhận `.alreadyInitialized`.
/// - `events` chỉ hỗ trợ một consumer (AppState lo fan-out) và phải bắt đầu đọc trước khi `login`.
public actor CoreBridge {
    public nonisolated let events: AsyncStream<CoreEvent>
    private let hub: Hub

    public init(config: CoreConfig) throws {
        let (stream, cont) = AsyncStream<CoreEvent>.makeStream()
        let hub = Hub(cont)
        guard let data = try? JSONEncoder().encode(config), let json = String(data: data, encoding: .utf8) else {
            throw CoreBridgeError.encodingFailed
        }
        let claimed = currentHub.withLock { cur -> Bool in
            guard cur == nil else { return false }
            cur = hub
            return true
        }
        guard claimed else { throw CoreBridgeError.alreadyInitialized }
        let code = core_init(json, coreEventCallback)
        if code != 0 {
            currentHub.withLock { if $0 === hub { $0 = nil } }
            switch code {
            case -1: throw CoreBridgeError.invalidConfig
            case -2: throw CoreBridgeError.unsupportedBackend
            case -3: throw CoreBridgeError.alreadyInitialized
            case -4: throw CoreBridgeError.shuttingDown
            default: throw CoreBridgeError.unknownInitCode(code)
            }
        }
        self.events = stream
        self.hub = hub
    }

    public nonisolated func call(_ cmd: CoreCommand) async throws -> CoreResponse {
        try await callRaw(cmd: cmd.name, args: cmd.args)
    }

    /// Đường gửi lệnh thô (test lệnh lạ).
    nonisolated func callRaw(cmd: String, args: [String: JSONValue]) async throws -> CoreResponse {
        let body: [String: JSONValue] = ["cmd": .string(cmd), "args": .object(args)]
        guard let data = try? JSONEncoder().encode(body), let json = String(data: data, encoding: .utf8) else {
            throw CoreBridgeError.encodingFailed
        }
        return try await hub.call(json)
    }

    public nonisolated func shutdown() async {
        await hub.shutdown()
    }
}

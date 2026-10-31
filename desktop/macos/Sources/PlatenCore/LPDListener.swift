import Foundation
import Network

/// Accepts LPD connections on the loopback interface.
public final class LPDListener {
    public static let defaultPort: UInt16 = 6515

    private let port: UInt16
    private let directory: URL
    private let onJob: (LPDJob) async -> Void
    private let onError: (String) -> Void
    private var listener: NWListener?
    private let queue = DispatchQueue(label: "platen.lpd")

    /// onJob receives each job; it owns the data files afterwards.
    public init(port: UInt16 = defaultPort, directory: URL,
                onJob: @escaping (LPDJob) async -> Void, onError: @escaping (String) -> Void) {
        self.port = port
        self.directory = directory
        self.onJob = onJob
        self.onError = onError
    }

    public func start() throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port)!)
        params.allowLocalEndpointReuse = true
        let l = try NWListener(using: params)
        l.newConnectionHandler = { [weak self] conn in self?.handle(conn) }
        l.stateUpdateHandler = { [weak self] state in
            if case .failed(let err) = state {
                self?.onError("The print receiver on 127.0.0.1:\(self?.port ?? 0) failed: \(err)")
            }
        }
        l.start(queue: queue)
        listener = l
    }

    public func stop() {
        listener?.cancel()
        listener = nil
    }

    private func handle(_ conn: NWConnection) {
        // Only local clients: the listener is bound to 127.0.0.1, but check anyway.
        if case .hostPort(let host, _) = conn.endpoint, "\(host)" != "127.0.0.1" {
            conn.cancel()
            return
        }
        conn.start(queue: queue)
        let stream = ConnectionStream(conn)
        let session = LPDSession(stream: stream, directory: directory)
        Task {
            do {
                for job in try await session.run() {
                    await onJob(job)
                }
            } catch {
                onError("\(error)")
            }
            conn.cancel()
        }
    }
}

final class ConnectionStream: LPDStream {
    private let conn: NWConnection

    init(_ conn: NWConnection) { self.conn = conn }

    func read(max: Int) async throws -> Data {
        try await withCheckedThrowingContinuation { cont in
            conn.receive(minimumIncompleteLength: 1, maximumLength: max) { data, _, isComplete, error in
                if let error {
                    cont.resume(throwing: error)
                } else if let data, !data.isEmpty {
                    cont.resume(returning: data)
                } else if isComplete {
                    cont.resume(returning: Data())
                } else {
                    cont.resume(returning: Data())
                }
            }
        }
    }

    func write(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            conn.send(content: data, completion: .contentProcessed { error in
                if let error { cont.resume(throwing: error) } else { cont.resume() }
            })
        }
    }
}

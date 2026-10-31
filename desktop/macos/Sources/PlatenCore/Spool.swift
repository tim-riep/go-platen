import Foundation

// Jobs received from CUPS wait in a local spool until the server accepted
// them, so printing works while the server is briefly unreachable.

public struct SpoolJob: Codable, Identifiable, Equatable {
    public var id: UUID
    public var printerID: String
    public var title: String
    public var contentType: String
    public var createdAt: Date
    public var attempts = 0
    public var nextAttempt: Date
    public var lastError: String?
    /// Set once the job will not be sent again.
    public var failed = false
}

public final class Spool: @unchecked Sendable {
    public let directory: URL
    private let lock = NSLock()

    public init(directory: URL) throws {
        self.directory = directory
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    public func documentURL(_ id: UUID) -> URL { directory.appendingPathComponent("\(id.uuidString).doc") }
    private func metaURL(_ id: UUID) -> URL { directory.appendingPathComponent("\(id.uuidString).json") }

    /// Moves the document into the spool and records the job.
    @discardableResult
    public func add(printerID: String, title: String, document: URL, now: Date = Date()) throws -> SpoolJob {
        let job = SpoolJob(id: UUID(), printerID: printerID, title: title,
                           contentType: Self.contentType(of: document), createdAt: now, nextAttempt: now)
        try FileManager.default.moveItem(at: document, to: documentURL(job.id))
        try save(job)
        return job
    }

    public func save(_ job: SpoolJob) throws {
        let enc = JSONEncoder()
        lock.lock()
        defer { lock.unlock() }
        try enc.encode(job).write(to: metaURL(job.id), options: .atomic)
    }

    public func remove(_ id: UUID) {
        lock.lock()
        defer { lock.unlock() }
        try? FileManager.default.removeItem(at: metaURL(id))
        try? FileManager.default.removeItem(at: documentURL(id))
    }

    /// All jobs, oldest first.
    public func jobs() -> [SpoolJob] {
        let dec = JSONDecoder()
        lock.lock()
        defer { lock.unlock() }
        let names = (try? FileManager.default.contentsOfDirectory(atPath: directory.path)) ?? []
        return names.filter { $0.hasSuffix(".json") }
            .compactMap { try? dec.decode(SpoolJob.self, from: Data(contentsOf: directory.appendingPathComponent($0))) }
            .sorted { $0.createdAt < $1.createdAt }
    }

    /// The MIME type by the document's first bytes. The app's PPD makes CUPS
    /// deliver PDF; PostScript is accepted for clients that send it directly.
    public static func contentType(of url: URL) -> String {
        guard let fh = try? FileHandle(forReadingFrom: url) else { return "application/octet-stream" }
        defer { try? fh.close() }
        let head = (try? fh.read(upToCount: 8)) ?? Data()
        if head.starts(with: Data("%PDF-".utf8)) { return "application/pdf" }
        if head.starts(with: Data("%!".utf8)) { return "application/postscript" }
        return "application/octet-stream"
    }
}

/// Sends spooled jobs to the server.
public actor Forwarder {
    public enum Event {
        case sent(SpoolJob, remoteID: String)
        case failed(SpoolJob)
        case waiting(SpoolJob)
    }

    private let spool: Spool
    private var client: ServerClient?
    private let onEvent: @Sendable (Event) -> Void
    private var running = false
    /// Jobs are given up after this time.
    public var maxAge: TimeInterval = 24 * 3600

    public init(spool: Spool, onEvent: @escaping @Sendable (Event) -> Void) {
        self.spool = spool
        self.onEvent = onEvent
    }

    public func setClient(_ c: ServerClient?) {
        client = c
    }

    /// Makes all waiting jobs due now.
    public func retryNow() {
        for var job in spool.jobs() where !job.failed {
            job.nextAttempt = Date()
            try? spool.save(job)
        }
    }

    /// Sends the due jobs; call it after adding a job and periodically.
    public func run(now: Date = Date()) async {
        guard !running, let client else { return }
        running = true
        defer { running = false }
        for var job in spool.jobs() where !job.failed && job.nextAttempt <= now {
            do {
                let remote = try await client.submit(printerID: job.printerID, title: job.title,
                                                     document: spool.documentURL(job.id), contentType: job.contentType,
                                                     idempotencyKey: "macos-" + job.id.uuidString)
                spool.remove(job.id)
                onEvent(.sent(job, remoteID: remote.id))
            } catch {
                let err = error as? ServerError ?? .transport("\(error)")
                job.attempts += 1
                job.lastError = err.description
                if !err.isRetryable || now.timeIntervalSince(job.createdAt) > maxAge {
                    job.failed = true
                    onEvent(.failed(job))
                } else {
                    job.nextAttempt = now.addingTimeInterval(Self.backoff(job.attempts))
                    if job.attempts == 1 { onEvent(.waiting(job)) }
                }
                try? spool.save(job)
            }
        }
    }

    static func backoff(_ attempts: Int) -> TimeInterval {
        min(300, 5 * pow(2, Double(min(attempts, 10) - 1)))
    }
}

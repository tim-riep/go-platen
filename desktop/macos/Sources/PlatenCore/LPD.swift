import Foundation

// Receiving side of the Line Printer Daemon protocol (RFC 1179), as far as
// the CUPS lpd backend uses it: "receive a printer job" with a control file
// and data files. The CUPS queues of the app point at this receiver on the
// loopback interface; the queue name in the URI is the platen printer ID.

/// Byte stream of one LPD connection.
public protocol LPDStream: AnyObject {
    /// Returns up to max bytes, or an empty Data at the end of the stream.
    func read(max: Int) async throws -> Data
    func write(_ data: Data) async throws
}

public enum LPDError: Error, CustomStringConvertible {
    case protocolError(String)
    case tooLarge(String)

    public var description: String {
        switch self {
        case .protocolError(let s): return "LPD protocol error: \(s)"
        case .tooLarge(let s): return "LPD: \(s) too large"
        }
    }
}

/// A received job. The data files are stored in the session's directory.
public struct LPDJob {
    public var queue: String
    public var control: ControlFile
    public var dataFiles: [URL]
}

/// The parts of an LPD control file platen uses.
public struct ControlFile: Equatable {
    public var host = ""
    public var user = ""
    public var jobName = ""
    public var sourceName = ""
    /// Data file names in print order, with the number of print lines each.
    public var prints: [(name: String, count: Int)] = []

    public static func == (a: ControlFile, b: ControlFile) -> Bool {
        a.host == b.host && a.user == b.user && a.jobName == b.jobName && a.sourceName == b.sourceName &&
            a.prints.map(\.name) == b.prints.map(\.name) && a.prints.map(\.count) == b.prints.map(\.count)
    }

    /// The job title shown in platen.
    public var title: String {
        for s in [jobName, sourceName] where !s.trimmingCharacters(in: .whitespaces).isEmpty {
            return s
        }
        return "Untitled"
    }

    // Commands that print a data file (RFC 1179, section 7).
    private static let printCommands: Set<Character> = ["c", "d", "f", "g", "l", "n", "o", "p", "r", "t", "v"]

    public init() {}

    public init(_ data: Data) {
        let text = String(decoding: data, as: UTF8.self)
        for line in text.split(whereSeparator: { $0 == "\n" || $0 == "\r" }) {
            guard let cmd = line.first else { continue }
            let value = String(line.dropFirst())
            switch cmd {
            case "H": host = value
            case "P": user = value
            case "J": jobName = value
            case "N": sourceName = value
            default:
                guard Self.printCommands.contains(cmd), !value.isEmpty else { continue }
                if let i = prints.firstIndex(where: { $0.name == value }) {
                    prints[i].count += 1
                } else {
                    prints.append((value, 1))
                }
            }
        }
    }
}

public struct LPDLimits {
    public var maxLine = 1024
    public var maxControlFile = 64 << 10
    public var maxDataFile: Int64 = 512 << 20
    public init() {}
}

/// One LPD connection.
public final class LPDSession {
    private let stream: LPDStream
    private let directory: URL
    private let limits: LPDLimits
    private var buffer = Data()
    private var eof = false

    /// directory receives the data files; it must exist.
    public init(stream: LPDStream, directory: URL, limits: LPDLimits = LPDLimits()) {
        self.stream = stream
        self.directory = directory
        self.limits = limits
    }

    /// Handles the connection and returns the jobs it delivered.
    public func run() async throws -> [LPDJob] {
        guard let line = try await readLine(), let cmd = line.first else { return [] }
        let operand = String(decoding: line.dropFirst(), as: UTF8.self)
        switch cmd {
        case 0x02:
            let queue = operand.split(separator: " ").first.map(String.init) ?? ""
            guard !queue.isEmpty else {
                try await nak()
                return []
            }
            try await ack()
            return try await receiveJob(queue: queue)
        case 0x03, 0x04:
            // Queue state: there is no local queue, jobs are forwarded at once.
            try await stream.write(Data("no entries\n".utf8))
            return []
        default:
            // Print waiting jobs, remove jobs: nothing to do.
            try await ack()
            return []
        }
    }

    private func receiveJob(queue: String) async throws -> [LPDJob] {
        var controls: [ControlFile] = []
        var data: [String: URL] = [:]
        var order: [String] = []
        while let line = try await readLine() {
            guard let sub = line.first else { continue }
            let fields = String(decoding: line.dropFirst(), as: UTF8.self).split(separator: " ", maxSplits: 1)
            switch sub {
            case 0x01:
                // Abort job.
                for url in data.values { try? FileManager.default.removeItem(at: url) }
                controls.removeAll()
                data.removeAll()
                order.removeAll()
                try await ack()
            case 0x02, 0x03:
                guard fields.count == 2, let count = Int64(fields[0]), count >= 0 else {
                    try await nak()
                    throw LPDError.protocolError("bad subcommand \(String(decoding: line, as: UTF8.self).debugDescription)")
                }
                let name = String(fields[1])
                try await ack()
                if sub == 0x02 {
                    guard count <= limits.maxControlFile else { throw LPDError.tooLarge("control file") }
                    let body = try await readExactly(Int(count))
                    try await expectTerminator()
                    controls.append(ControlFile(body))
                } else {
                    let url = directory.appendingPathComponent(UUID().uuidString)
                    try await receiveFile(count: count, to: url)
                    if let old = data[name] { try? FileManager.default.removeItem(at: old) }
                    data[name] = url
                    if !order.contains(name) { order.append(name) }
                }
                try await ack()
            default:
                try await nak()
                throw LPDError.protocolError("unknown subcommand \(sub)")
            }
        }
        guard !data.isEmpty else { return [] }
        if controls.isEmpty { controls.append(ControlFile()) }
        var jobs: [LPDJob] = []
        var used = Set<String>()
        for control in controls {
            var names = control.prints.map(\.name).filter { data[$0] != nil }
            if names.isEmpty { names = order.filter { !used.contains($0) } }
            used.formUnion(names)
            if !names.isEmpty {
                jobs.append(LPDJob(queue: queue, control: control, dataFiles: names.compactMap { data[$0] }))
            }
        }
        // Data files no control file refers to are dropped.
        for (name, url) in data where !used.contains(name) {
            try? FileManager.default.removeItem(at: url)
        }
        return jobs
    }

    /// Copies a data file to url. A count of 0 means "until the end of the
    /// connection" (used by some clients for streamed jobs).
    private func receiveFile(count: Int64, to url: URL) async throws {
        FileManager.default.createFile(atPath: url.path, contents: nil)
        let fh = try FileHandle(forWritingTo: url)
        defer { try? fh.close() }
        var remaining = count
        var written: Int64 = 0
        while count == 0 || remaining > 0 {
            if buffer.isEmpty {
                guard try await fill() else {
                    if count == 0 { return }
                    throw LPDError.protocolError("connection closed in data file")
                }
            }
            let n = count == 0 ? buffer.count : Int(min(Int64(buffer.count), remaining))
            try fh.write(contentsOf: buffer.prefix(n))
            buffer.removeFirst(n)
            remaining -= Int64(n)
            written += Int64(n)
            if written > limits.maxDataFile { throw LPDError.tooLarge("data file") }
        }
        try await expectTerminator()
    }

    private func expectTerminator() async throws {
        let t = try await readExactly(1)
        guard t.first == 0 else { throw LPDError.protocolError("missing file terminator") }
    }

    private func ack() async throws { try await stream.write(Data([0])) }
    private func nak() async throws { try await stream.write(Data([1])) }

    private func fill() async throws -> Bool {
        if eof { return false }
        let d = try await stream.read(max: 256 << 10)
        if d.isEmpty {
            eof = true
            return false
        }
        buffer.append(d)
        return true
    }

    /// Returns the next line without the newline, or nil at the end.
    private func readLine() async throws -> Data? {
        while true {
            if let i = buffer.firstIndex(of: 0x0A) {
                let line = buffer[buffer.startIndex..<i]
                buffer.removeSubrange(buffer.startIndex...i)
                return Data(line)
            }
            guard buffer.count <= limits.maxLine else { throw LPDError.tooLarge("command line") }
            guard try await fill() else {
                if buffer.isEmpty { return nil }
                throw LPDError.protocolError("connection closed in a command")
            }
        }
    }

    private func readExactly(_ n: Int) async throws -> Data {
        while buffer.count < n {
            guard try await fill() else { throw LPDError.protocolError("connection closed early") }
        }
        let d = Data(buffer.prefix(n))
        buffer.removeFirst(n)
        return d
    }
}

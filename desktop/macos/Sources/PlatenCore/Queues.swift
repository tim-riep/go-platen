import Foundation

// Local CUPS queues for the platen printers. Each queue sends its jobs as
// PDF (see platen.ppd) to the app's LPD receiver; the queue name in the
// device URI is the platen printer ID. The app manages only queues whose
// device URI points at its receiver.

public struct LocalQueue: Equatable {
    public var name: String
    public var printerID: String
    public var description: String
}

public enum QueueAction: Equatable {
    case add(RemotePrinter)
    case update(RemotePrinter)
    case remove(String) // queue name
}

public enum QueueError: Error, CustomStringConvertible {
    case command(String, String)
    case canceled

    public var description: String {
        switch self {
        case .command(let cmd, let out): return "\(cmd) failed: \(out)"
        case .canceled: return "The administrator authorization was canceled"
        }
    }
}

public struct Queues {
    public static let prefix = "platen_"
    public let port: UInt16
    public let ppd: URL
    /// Shown as the location of each queue.
    public var location = "platen"

    public init(port: UInt16 = LPDListener.defaultPort, ppd: URL) {
        self.port = port
        self.ppd = ppd
    }

    public static func queueName(for printerID: String) -> String { prefix + printerID }

    public func deviceURI(for printerID: String) -> String {
        "lpd://127.0.0.1:\(port)/\(printerID)?reserve=none"
    }

    /// The CUPS page size for platen's paper size.
    public static func pageSize(_ paper: String) -> String? {
        ["a4": "A4", "a5": "A5", "letter": "Letter", "legal": "Legal"][paper.lowercased()]
    }

    // MARK: Reading

    /// The queues of this app, from `lpstat -v` and `lpoptions -p`.
    public func installed() throws -> [LocalQueue] {
        let devices = try Self.run("/usr/bin/lpstat", ["-v"], allowFailure: true)
        var queues: [LocalQueue] = []
        for (name, uri) in Self.parseDevices(devices) {
            guard name.hasPrefix(Self.prefix), let id = printerID(fromURI: uri) else { continue }
            let options = Self.parseOptions(try Self.run("/usr/bin/lpoptions", ["-p", name], allowFailure: true))
            queues.append(LocalQueue(name: name, printerID: id, description: options["printer-info"] ?? ""))
        }
        return queues
    }

    func printerID(fromURI uri: String) -> String? {
        let base = "lpd://127.0.0.1:\(port)/"
        guard uri.hasPrefix(base) else { return nil }
        let rest = uri.dropFirst(base.count)
        let id = rest.split(separator: "?", maxSplits: 1).first.map(String.init) ?? ""
        return id.isEmpty ? nil : id
    }

    /// Parses the lines of `lpstat -v`, "device for NAME: URI". CUPS
    /// translates the text before the name, so only the shape is used.
    static func parseDevices(_ out: String) -> [(String, String)] {
        out.split(separator: "\n").compactMap { line in
            guard let sep = line.range(of: ": ") else { return nil }
            let uri = line[sep.upperBound...].trimmingCharacters(in: .whitespaces)
            guard uri.contains(":"), let name = line[..<sep.lowerBound].split(separator: " ").last else { return nil }
            return (String(name), uri)
        }
    }

    /// Parses the key=value list of `lpoptions -p`; values may be quoted
    /// with ' or " and contain backslash escapes.
    static func parseOptions(_ out: String) -> [String: String] {
        var result: [String: String] = [:]
        var it = out.trimmingCharacters(in: .whitespacesAndNewlines).makeIterator()
        var key = "", value = "", inValue = false
        var quote: Character?
        func flush() {
            if !key.isEmpty { result[key] = value }
            key = ""
            value = ""
            inValue = false
        }
        while let c = it.next() {
            if c == "\\", let n = it.next() {
                if inValue { value.append(n) } else { key.append(n) }
            } else if let q = quote {
                if c == q { quote = nil } else { value.append(c) }
            } else if inValue && (c == "'" || c == "\"") {
                quote = c
            } else if c == " " {
                flush()
            } else if !inValue && c == "=" {
                inValue = true
            } else if inValue {
                value.append(c)
            } else {
                key.append(c)
            }
        }
        flush()
        return result
    }

    // MARK: Changing

    /// What to change so the queues match the printers.
    public static func plan(installed: [LocalQueue], printers: [RemotePrinter]) -> [QueueAction] {
        var actions: [QueueAction] = []
        let byID = Dictionary(installed.map { ($0.printerID, $0) }, uniquingKeysWith: { a, _ in a })
        for p in printers where p.acceptsPDF {
            if let q = byID[p.id] {
                if q.description != p.name { actions.append(.update(p)) }
            } else {
                actions.append(.add(p))
            }
        }
        let wanted = Set(printers.filter(\.acceptsPDF).map(\.id))
        for q in installed where !wanted.contains(q.printerID) || q.name != queueName(for: q.printerID) {
            actions.append(.remove(q.name))
        }
        return actions
    }

    public func arguments(for action: QueueAction) -> [String] {
        switch action {
        case .add(let p):
            var args = ["-p", Self.queueName(for: p.id), "-D", p.name, "-L", location,
                        "-v", deviceURI(for: p.id), "-P", ppd.path, "-E",
                        "-o", "printer-is-shared=false", "-o", "printer-error-policy=retry-job"]
            if let size = Self.pageSize(p.paperSize) { args += ["-o", "PageSize=\(size)"] }
            return args
        case .update(let p):
            return ["-p", Self.queueName(for: p.id), "-D", p.name, "-L", location]
        case .remove(let name):
            return ["-x", name]
        }
    }

    /// Applies the actions with lpadmin. Members of the lpadmin group (admin
    /// users) need no password; otherwise macOS asks for an administrator once.
    public func apply(_ actions: [QueueAction]) throws {
        guard !actions.isEmpty else { return }
        do {
            for a in actions {
                try Self.run("/usr/sbin/lpadmin", arguments(for: a))
            }
        } catch QueueError.command(_, let out) where Self.isPermissionError(out) {
            let script = actions.map { (["/usr/sbin/lpadmin"] + arguments(for: $0)).map(Self.shellQuote).joined(separator: " ") }
                .joined(separator: " && ")
            try Self.runPrivileged(script)
        }
    }

    static func isPermissionError(_ out: String) -> Bool {
        let s = out.lowercased()
        return s.contains("forbidden") || s.contains("not authorized") || s.contains("unauthorized")
    }

    static func shellQuote(_ s: String) -> String {
        "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    static func runPrivileged(_ script: String) throws {
        let escaped = script.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
        do {
            try run("/usr/bin/osascript", ["-e", "do shell script \"\(escaped)\" with administrator privileges"])
        } catch QueueError.command(_, let out) where out.contains("-128") {
            throw QueueError.canceled
        }
    }

    @discardableResult
    static func run(_ path: String, _ args: [String], allowFailure: Bool = false) throws -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = args
        var env = ProcessInfo.processInfo.environment
        env["LC_ALL"] = "C"
        env["LANG"] = "C"
        p.environment = env
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let text = String(decoding: data, as: UTF8.self)
        if p.terminationStatus != 0 && !allowFailure {
            let cmd = URL(fileURLWithPath: path).lastPathComponent
            throw QueueError.command(cmd, text.trimmingCharacters(in: .whitespacesAndNewlines))
        }
        return text
    }
}

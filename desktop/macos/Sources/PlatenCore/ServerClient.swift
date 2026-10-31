import Foundation

// Client of the platen server's client API (/client/v1). The server knows
// the device by its NetBird peer, so there is no token.

public struct ClientMe: Decodable, Equatable {
    public var ip: String
    public var fqdn: String
    public var netbirdUser: String?
    public var panelRole: String?
    public var version: String

    enum CodingKeys: String, CodingKey {
        case ip, fqdn, version
        case netbirdUser = "netbird_user"
        case panelRole = "panel_role"
    }
}

public struct RemotePrinter: Decodable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var status: String
    public var reachable: Bool?
    public var paperSize: String
    public var resolutionDPI: Int
    public var contentTypes: [String]?

    public init(id: String, name: String, status: String = "ready", reachable: Bool? = nil,
                paperSize: String = "A4", resolutionDPI: Int = 600, contentTypes: [String]? = nil) {
        self.id = id
        self.name = name
        self.status = status
        self.reachable = reachable
        self.paperSize = paperSize
        self.resolutionDPI = resolutionDPI
        self.contentTypes = contentTypes
    }

    enum CodingKeys: String, CodingKey {
        case id, name, status, reachable
        case paperSize = "paper_size"
        case resolutionDPI = "resolution_dpi"
        case contentTypes = "content_types"
    }

    /// Whether the printer's backend takes PDF documents.
    public var acceptsPDF: Bool {
        contentTypes.map { $0.contains("application/pdf") } ?? true
    }
}

public struct RemoteJob: Decodable {
    public var id: String
    public var status: String
    public var error: String?
}

public enum ServerError: Error, CustomStringConvertible {
    case badAddress(String)
    case http(status: Int, code: String, message: String)
    case transport(String)
    case decoding(String)

    public var description: String {
        switch self {
        case .badAddress(let s): return "Invalid server address \"\(s)\""
        case .http(let status, _, let message): return message.isEmpty ? "HTTP \(status)" : message
        case .transport(let s): return s
        case .decoding(let s): return "Unexpected server response: \(s)"
        }
    }

    /// Whether sending the same request later may succeed.
    public var isRetryable: Bool {
        switch self {
        case .http(let status, _, _):
            // 403: the server may not know the device yet (NetBird user sync).
            return status == 403 || status == 408 || status == 429 || status >= 500
        case .transport: return true
        default: return false
        }
    }
}

struct ErrorBody: Decodable {
    struct Detail: Decodable {
        var code: String
        var message: String
    }
    var error: Detail
}

public final class ServerClient {
    public let baseURL: URL
    private let session: URLSession
    private let clientName: String

    /// Accepts "host", "host:port" or "http://host[:port]".
    public static func url(for address: String) throws -> URL {
        var s = address.trimmingCharacters(in: .whitespacesAndNewlines)
        if !s.contains("://") { s = "http://" + s }
        if s.hasSuffix("/") && !s.hasSuffix("://") { s.removeLast() }
        guard let u = URLComponents(string: s), let host = u.host, !host.isEmpty,
              u.scheme == "http" || u.scheme == "https", u.path.isEmpty, u.query == nil
        else { throw ServerError.badAddress(address) }
        var c = URLComponents()
        c.scheme = u.scheme
        c.host = host
        c.port = u.port
        return c.url!
    }

    public init(address: String, clientName: String, session: URLSession = .shared) throws {
        baseURL = try Self.url(for: address)
        self.clientName = clientName
        self.session = session
    }

    public func me() async throws -> ClientMe {
        try await get("/client/v1/me")
    }

    public func printers() async throws -> [RemotePrinter] {
        struct List: Decodable { var printers: [RemotePrinter] }
        let l: List = try await get("/client/v1/printers")
        return l.printers
    }

    /// Submits the document. The idempotency key makes retries safe.
    public func submit(printerID: String, title: String, document: URL, contentType: String,
                       idempotencyKey: String) async throws -> RemoteJob {
        var c = URLComponents(url: baseURL.appendingPathComponent("/client/v1/printers/\(printerID)/jobs"),
                              resolvingAgainstBaseURL: false)!
        c.queryItems = [URLQueryItem(name: "title", value: Self.truncate(title, bytes: 255))]
        var req = request(c.url!)
        req.httpMethod = "POST"
        req.setValue(contentType, forHTTPHeaderField: "Content-Type")
        req.setValue(idempotencyKey, forHTTPHeaderField: "Idempotency-Key")
        req.timeoutInterval = 300
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.upload(for: req, fromFile: document)
        } catch {
            throw ServerError.transport(error.localizedDescription)
        }
        return try decode(data, resp)
    }

    private func request(_ url: URL) -> URLRequest {
        var req = URLRequest(url: url)
        req.setValue(clientName, forHTTPHeaderField: "X-Platen-Client")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        req.timeoutInterval = 20
        return req
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.data(for: request(baseURL.appendingPathComponent(path)))
        } catch {
            throw ServerError.transport(error.localizedDescription)
        }
        return try decode(data, resp)
    }

    private func decode<T: Decodable>(_ data: Data, _ resp: URLResponse) throws -> T {
        guard let http = resp as? HTTPURLResponse else { throw ServerError.decoding("no HTTP response") }
        guard (200..<300).contains(http.statusCode) else {
            if let b = try? JSONDecoder().decode(ErrorBody.self, from: data) {
                throw ServerError.http(status: http.statusCode, code: b.error.code, message: b.error.message)
            }
            throw ServerError.http(status: http.statusCode, code: "", message: "")
        }
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw ServerError.decoding("\(error)")
        }
    }

    static func truncate(_ s: String, bytes: Int) -> String {
        var out = ""
        for ch in s {
            if out.utf8.count + String(ch).utf8.count > bytes { break }
            out.append(ch)
        }
        return out
    }
}

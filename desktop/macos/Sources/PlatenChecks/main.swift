import Foundation
@testable import PlatenCore

// Checks of PlatenCore without XCTest: swift run PlatenChecks

var failures = 0

func check(_ cond: Bool, _ what: String, file: String = #file, line: Int = #line) {
    if !cond {
        failures += 1
        print("FAIL \(URL(fileURLWithPath: file).lastPathComponent):\(line): \(what)")
    }
}

final class MemoryStream: LPDStream {
    var input: [Data]
    var output = Data()
    init(_ chunks: [Data]) { input = chunks }
    func read(max: Int) async throws -> Data {
        guard !input.isEmpty else { return Data() }
        var d = input.removeFirst()
        if d.count > max {
            input.insert(d.suffix(from: d.startIndex + max), at: 0)
            d = d.prefix(max)
        }
        return d
    }
    func write(_ data: Data) async throws { output.append(data) }
}

/// Splits the bytes into chunks of n bytes to exercise partial reads.
func chunks(_ d: Data, _ n: Int) -> [Data] {
    stride(from: 0, to: d.count, by: n).map { Data(d[$0..<min($0 + n, d.count)]) }
}

func lpdBytes(queue: String, control: String, data: Data, dataLength: Int? = nil) -> Data {
    var b = Data("\u{02}\(queue)\n".utf8)
    b.append(Data("\u{02}\(control.utf8.count) cfA001host\n".utf8))
    b.append(Data(control.utf8))
    b.append(0)
    b.append(Data("\u{03}\(dataLength ?? data.count) dfA001host\n".utf8))
    b.append(data)
    if dataLength != 0 { b.append(0) }
    return b
}

let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("platen-checks-\(UUID().uuidString)")
try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
defer { try? FileManager.default.removeItem(at: tmp) }

// Control file.
let cf = ControlFile(Data("Hmac\nPtim\nJQuarterly report\nldfA001host\nldfA001host\nNreport.pdf\nUdfA001host\n".utf8))
check(cf.user == "tim" && cf.host == "mac" && cf.title == "Quarterly report", "control file fields \(cf)")
check(cf.prints.count == 1 && cf.prints[0].count == 2, "print lines \(cf.prints)")
check(ControlFile(Data("NFile.pdf\n".utf8)).title == "File.pdf", "title falls back to N")
check(ControlFile(Data("J  \n".utf8)).title == "Untitled", "blank title")

// A job as CUPS sends it, delivered in small pieces.
let pdf = Data("%PDF-1.3\n".utf8) + Data(repeating: 0x41, count: 5000)
for size in [1, 7, 4096, 1 << 20] {
    let s = MemoryStream(chunks(lpdBytes(queue: "office", control: "Ptim\nJDoc\nldfA001host\n", data: pdf), size))
    let jobs = try await LPDSession(stream: s, directory: tmp).run()
    check(jobs.count == 1 && jobs[0].queue == "office" && jobs[0].control.title == "Doc", "job (chunks of \(size))")
    if let j = jobs.first {
        check(try Data(contentsOf: j.dataFiles[0]) == pdf, "data file content (chunks of \(size))")
        check(Spool.contentType(of: j.dataFiles[0]) == "application/pdf", "content type")
    }
    check(s.output == Data([0, 0, 0, 0, 0]), "acks \(Array(s.output))")
}

// A data file with length 0 runs to the end of the connection.
do {
    let s = MemoryStream([lpdBytes(queue: "q", control: "JX\nldfA001host\n", data: pdf, dataLength: 0)])
    let jobs = try await LPDSession(stream: s, directory: tmp).run()
    check(jobs.count == 1 && (try? Data(contentsOf: jobs[0].dataFiles[0])) == pdf, "streamed data file")
}

// Limits and malformed input.
do {
    var limits = LPDLimits()
    limits.maxDataFile = 100
    let s = MemoryStream([lpdBytes(queue: "q", control: "JX\n", data: pdf)])
    var threw = false
    do { _ = try await LPDSession(stream: s, directory: tmp, limits: limits).run() } catch { threw = true }
    check(threw, "data file limit")

    let bad = MemoryStream([Data("\u{02}q\n\u{02}x cfA\n".utf8)])
    threw = false
    do { _ = try await LPDSession(stream: bad, directory: tmp).run() } catch { threw = true }
    check(threw && bad.output == Data([0, 1]), "bad length is rejected \(Array(bad.output))")

    let state = MemoryStream([Data("\u{03}q\n".utf8)])
    check(try await LPDSession(stream: state, directory: tmp).run().isEmpty && !state.output.isEmpty, "queue state")

    let aborted = MemoryStream([Data("\u{02}q\n".utf8) + Data("\u{03}3 dfA\nabc\0\u{01}\n".utf8)])
    check(try await LPDSession(stream: aborted, directory: tmp).run().isEmpty, "aborted job")
}

// Server addresses.
for (input, want) in [("printserver", "http://printserver"), ("printserver.netbird.cloud/", "http://printserver.netbird.cloud"),
                      ("100.64.0.1:8080", "http://100.64.0.1:8080"), ("https://ps.example", "https://ps.example")] {
    check((try? ServerClient.url(for: input).absoluteString) == want, "address \(input)")
}
for input in ["", "ftp://x", "http://x/path", "http://"] {
    check((try? ServerClient.url(for: input)) == nil, "bad address \(input)")
}
check(try ServerClient.url(for: "ps").appendingPathComponent("/client/v1/me").absoluteString == "http://ps/client/v1/me", "path join")
check(ServerClient.truncate("aä" + String(repeating: "x", count: 300), bytes: 255).utf8.count == 255, "title truncation")
check(ServerClient.truncate("ää", bytes: 3) == "ä", "truncation keeps characters whole")
check(ServerError.http(status: 403, code: "forbidden", message: "").isRetryable, "403 is retried")
check(!ServerError.http(status: 404, code: "not_found", message: "").isRetryable, "404 is final")

// Queues.
let queues = Queues(ppd: URL(fileURLWithPath: "/x/platen.ppd"))
let devices = Queues.parseDevices("""
device for Brother_HL_2130_series: dnssd://Brother._pdl-datastream._tcp.local./
device for platen_office: lpd://127.0.0.1:6515/office?reserve=none
device for platen_old: lpd://127.0.0.1:6515/old?reserve=none
device for platen_foreign: ipp://10.0.0.5/ipp
""")
check(devices.count == 4 && devices[1] == ("platen_office", "lpd://127.0.0.1:6515/office?reserve=none"), "lpstat -v \(devices)")
check(queues.printerID(fromURI: devices[1].1) == "office" && queues.printerID(fromURI: devices[3].1) == nil, "device URIs")
check(Queues.parseDevices("Gerät für platen_x: lpd://127.0.0.1:6515/x?reserve=none\nlpstat: Keine Ziele hinzugefügt.\n").map(\.0) == ["platen_x"], "localized lpstat -v")
check(Queues.parseOptions("device-uri=lpd://127.0.0.1:6515/x printer-info='Office \\'A\\' printer' printer-location=\"2nd floor\" printer-type=16810052\n")
    == ["device-uri": "lpd://127.0.0.1:6515/x", "printer-info": "Office 'A' printer", "printer-location": "2nd floor", "printer-type": "16810052"], "lpoptions -p")
let plan = Queues.plan(
    installed: [LocalQueue(name: "platen_office", printerID: "office", description: "Office"),
                LocalQueue(name: "platen_old", printerID: "old", description: "Old")],
    printers: [RemotePrinter(id: "office", name: "Office 2"), RemotePrinter(id: "lab", name: "Lab"),
               RemotePrinter(id: "zebra", name: "Labels", contentTypes: ["application/vnd.zebra-zpl"])])
check(plan == [.update(RemotePrinter(id: "office", name: "Office 2")), .add(RemotePrinter(id: "lab", name: "Lab")), .remove("platen_old")], "plan \(plan)")
check(queues.arguments(for: .add(RemotePrinter(id: "lab", name: "Lab", paperSize: "letter"))).suffix(2) == ["-o", "PageSize=Letter"], "page size")
check(Queues.shellQuote("it's") == "'it'\\''s'", "shell quoting")

// Spool and backoff.
do {
    let spool = try Spool(directory: tmp.appendingPathComponent("spool"))
    let doc = tmp.appendingPathComponent("doc")
    try pdf.write(to: doc)
    let job = try spool.add(printerID: "office", title: "Doc", document: doc)
    check(spool.jobs() == [job] && job.contentType == "application/pdf", "spool add")
    spool.remove(job.id)
    check(spool.jobs().isEmpty, "spool remove")
    check(Forwarder.backoff(1) == 5 && Forwarder.backoff(3) == 20 && Forwarder.backoff(20) == 300, "backoff")
}

if failures > 0 {
    print("\(failures) check(s) failed")
    exit(1)
}
print("all checks passed")

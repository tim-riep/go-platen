import Foundation
import Observation
import os
import PlatenCore
import ServiceManagement
import UserNotifications

/// State of the app: the server connection, the local queues and the spool.
let log = Logger(subsystem: "io.github.tim-riep.platen", category: "app")

@MainActor
@Observable
final class AppModel {
    static let version = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    private static let addressKey = "serverAddress"
    private static let syncInterval: TimeInterval = 300

    var address: String
    private(set) var connectedAddress: String?
    private(set) var me: ClientMe?
    private(set) var printers: [RemotePrinter] = []
    private(set) var queues: [LocalQueue] = []
    private(set) var jobs: [SpoolJob] = []
    private(set) var lastSync: Date?
    private(set) var error: String?
    private(set) var receiverError: String?
    private(set) var busy = false
    var launchAtLogin: Bool {
        get { SMAppService.mainApp.status == .enabled }
        set { setLaunchAtLogin(newValue) }
    }

    @ObservationIgnored nonisolated private let spool: Spool
    @ObservationIgnored nonisolated private let forwarder: Forwarder
    private var listener: LPDListener?
    private var timers: [Timer] = []
    private let isBundled = Bundle.main.bundleIdentifier != nil

    var configured: Bool { connectedAddress != nil }

    init() {
        let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Platen")
        // The spool lives in Application Support, which always exists.
        spool = try! Spool(directory: support.appendingPathComponent("spool"))
        let saved = UserDefaults.standard.string(forKey: Self.addressKey)
        address = saved ?? ""
        connectedAddress = saved
        weak var weakSelf: AppModel?
        forwarder = Forwarder(spool: spool) { event in
            Task { @MainActor in weakSelf?.handle(event) }
        }
        weakSelf = self

        let l = LPDListener(directory: support.appendingPathComponent("incoming"),
                            onJob: { job in await weakSelf?.received(job) },
                            onError: { msg in Task { @MainActor in weakSelf?.receiverFailed(msg) } })
        do {
            try l.start()
            listener = l
        } catch {
            receiverError = "The print receiver could not start: \(error)"
        }
        if isBundled {
            UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, _ in }
        }
        jobs = spool.jobs()
        timers.append(Timer.scheduledTimer(withTimeInterval: Self.syncInterval, repeats: true) { _ in
            Task { @MainActor in await weakSelf?.refresh() }
        })
        timers.append(Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { _ in
            Task { @MainActor in await weakSelf?.forward() }
        })
        Task { await refresh() }
    }

    // MARK: Server

    /// Connects to the server in `address` and installs its printers.
    func connect() async {
        let addr = address.trimmingCharacters(in: .whitespacesAndNewlines)
        busy = true
        defer { busy = false }
        do {
            try await sync(address: addr)
            let first = connectedAddress == nil
            connectedAddress = addr
            UserDefaults.standard.set(addr, forKey: Self.addressKey)
            if first && isBundled && SMAppService.mainApp.status != .enabled {
                // Printing needs the app running, so it starts at login by default.
                setLaunchAtLogin(true)
            }
        } catch {
            self.error = describe(error)
            log.error("\(String(describing: error), privacy: .public)")
        }
    }

    /// Updates printers and queues from the connected server.
    func refresh() async {
        guard let addr = connectedAddress, !busy else { return }
        busy = true
        defer { busy = false }
        do {
            try await sync(address: addr)
        } catch {
            self.error = describe(error)
            log.error("\(String(describing: error), privacy: .public)")
        }
    }

    private func sync(address addr: String) async throws {
        let client = try ServerClient(address: addr, clientName: "platen-macos/\(Self.version)")
        let me = try await client.me()
        let printers = try await client.printers()
        var q = Queues(ppd: try ppdURL())
        q.location = "platen on \(client.baseURL.host ?? addr)"
        let queues = q
        self.queues = try await Task.detached {
            try queues.apply(Queues.plan(installed: try queues.installed(), printers: printers))
            return try queues.installed()
        }.value
        self.me = me
        self.printers = printers
        self.error = nil
        self.lastSync = Date()
        log.info("synced \(printers.count) printer(s), \(self.queues.count) queue(s) installed")
        await forwarder.setClient(client)
        await forward()
    }

    /// Removes the app's queues and forgets the server.
    func disconnect() async {
        busy = true
        defer { busy = false }
        do {
            let q = Queues(ppd: try ppdURL())
            queues = try await Task.detached {
                try q.apply(try q.installed().map { .remove($0.name) })
                return try q.installed()
            }.value
            UserDefaults.standard.removeObject(forKey: Self.addressKey)
            connectedAddress = nil
            me = nil
            printers = []
            error = nil
            await forwarder.setClient(nil)
        } catch {
            self.error = describe(error)
            log.error("\(String(describing: error), privacy: .public)")
        }
    }

    private func ppdURL() throws -> URL {
        if let u = Bundle.main.url(forResource: "platen", withExtension: "ppd") { return u }
        if let p = ProcessInfo.processInfo.environment["PLATEN_PPD"] { return URL(fileURLWithPath: p) }
        throw ServerError.decoding("platen.ppd is missing from the app bundle")
    }

    // MARK: Jobs

    nonisolated private func received(_ job: LPDJob) async {
        for file in job.dataFiles {
            _ = try? spool.add(printerID: job.queue, title: job.control.title, document: file)
        }
        await MainActor.run { self.jobs = self.spool.jobs() }
        await forwarder.run()
        await MainActor.run { self.jobs = self.spool.jobs() }
    }

    func forward() async {
        await forwarder.run()
        jobs = spool.jobs()
    }

    func retryNow() async {
        await forwarder.retryNow()
        await forward()
    }

    func discard(_ job: SpoolJob) {
        spool.remove(job.id)
        jobs = spool.jobs()
    }

    private func handle(_ event: Forwarder.Event) {
        jobs = spool.jobs()
        log.info("job event: \(String(describing: event), privacy: .public)")
        switch event {
        case .sent:
            break
        case .waiting(let job):
            notify("“\(job.title)” is waiting", "The print server is not reachable. Platen keeps trying.")
        case .failed(let job):
            notify("“\(job.title)” was not printed", job.lastError ?? "The print server refused the job.")
        }
    }

    private func receiverFailed(_ msg: String) {
        log.error("receiver: \(msg, privacy: .public)")
        receiverError = msg
    }

    private func notify(_ title: String, _ body: String) {
        guard isBundled else { return }
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil))
    }

    // MARK: Misc

    private func setLaunchAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            self.error = "Open at login: \(error.localizedDescription)"
        }
    }

    private func describe(_ error: Error) -> String {
        switch error {
        case ServerError.http(403, _, let message):
            return message + ". Ask an administrator to give this device a role in the platen panel."
        case ServerError.transport(let message):
            return message + " Is NetBird connected?"
        default:
            return "\(error)"
        }
    }

    func isInstalled(_ p: RemotePrinter) -> Bool {
        queues.contains { $0.printerID == p.id }
    }
}

import PlatenCore
import SwiftUI

struct MainView: View {
    @Bindable var model: AppModel
    @State private var confirmDisconnect = false

    var body: some View {
        Form {
            Section {
                TextField("Server", text: $model.address, prompt: Text("printserver.netbird.cloud"))
                    .onSubmit { Task { await model.connect() } }
                    .disabled(model.busy)
                HStack {
                    Button(model.configured && model.address == model.connectedAddress ? "Update Printers" : "Connect & Install Printers") {
                        Task { await model.connect() }
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.busy || model.address.trimmingCharacters(in: .whitespaces).isEmpty)
                    if model.busy { ProgressView().controlSize(.small) }
                }
                if let me = model.me {
                    LabeledContent("This device", value: "\(me.fqdn) (\(me.ip))")
                    if let user = me.netbirdUser { LabeledContent("NetBird user", value: user) }
                    if let role = me.panelRole { LabeledContent("Panel role", value: role) }
                    LabeledContent("Server version", value: me.version)
                }
                if let error = model.error {
                    Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red)
                }
                if let error = model.receiverError {
                    Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red)
                }
            } header: {
                Text("Print server")
            } footer: {
                Text("The NetBird name or IP of the platen server. This Mac must be connected to NetBird.")
                    .foregroundStyle(.secondary)
            }

            if model.configured {
                Section("Printers") {
                    if model.printers.isEmpty {
                        Text("The server has no printers.").foregroundStyle(.secondary)
                    }
                    ForEach(model.printers) { p in
                        PrinterRow(printer: p, installed: model.isInstalled(p))
                    }
                    if let t = model.lastSync {
                        Text("Updated \(t.formatted(date: .omitted, time: .shortened)). Printers appear in every app’s print dialog.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            }

            if !model.jobs.isEmpty {
                Section("Jobs") {
                    ForEach(model.jobs) { job in
                        HStack {
                            VStack(alignment: .leading) {
                                Text(job.title)
                                Text(job.failed ? "Not printed: \(job.lastError ?? "")" : "Waiting: \(job.lastError ?? "sending")")
                                    .font(.caption).foregroundStyle(job.failed ? .red : .secondary)
                            }
                            Spacer()
                            Button("Discard") { model.discard(job) }
                        }
                    }
                    Button("Retry Now") { Task { await model.retryNow() } }
                        .disabled(!model.jobs.contains { !$0.failed })
                }
            }

            Section("General") {
                Toggle("Open at login", isOn: $model.launchAtLogin)
                if model.configured {
                    Button("Remove Printers & Disconnect…", role: .destructive) { confirmDisconnect = true }
                        .disabled(model.busy)
                }
            }
        }
        .formStyle(.grouped)
        .confirmationDialog("Remove the platen printers from this Mac?", isPresented: $confirmDisconnect) {
            Button("Remove", role: .destructive) { Task { await model.disconnect() } }
        }
    }
}

struct PrinterRow: View {
    let printer: RemotePrinter
    let installed: Bool

    var body: some View {
        HStack {
            Image(systemName: installed ? "checkmark.circle.fill" : "circle.dashed")
                .foregroundStyle(installed ? .green : .secondary)
            VStack(alignment: .leading) {
                Text(printer.name)
                Text(detail).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Text(state.0).font(.caption).foregroundStyle(state.1)
        }
    }

    private var detail: String {
        var parts = [printer.id, printer.paperSize]
        if !printer.acceptsPDF { parts.append("no PDF support, not installed") }
        return parts.joined(separator: " · ")
    }

    private var state: (String, Color) {
        if printer.status != "ready" { return (printer.status, .orange) }
        switch printer.reachable {
        case .some(false): return ("unreachable", .orange)
        case .some(true): return ("ready", .green)
        case .none: return ("ready", .secondary)
        }
    }
}

import AppKit
import PlatenCore
import SwiftUI

@main
struct PlatenApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            MenuLabel(model: model)
        }
        Window("Platen", id: "main") {
            MainView(model: model)
                .frame(minWidth: 520, minHeight: 480)
        }
        .windowResizability(.contentSize)
    }
}

struct MenuLabel: View {
    let model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(systemName: model.error != nil || model.jobs.contains(where: \.failed) ? "printer.dotmatrix" : "printer")
            .task {
                // Without a server there is nothing else to do: ask for one.
                if !model.configured { show(openWindow) }
            }
    }
}

@MainActor
func show(_ openWindow: OpenWindowAction) {
    openWindow(id: "main")
    NSApp.activate(ignoringOtherApps: true)
}

struct MenuContent: View {
    let model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        if let me = model.me, let addr = model.connectedAddress {
            Text("Connected to \(addr)")
            Text(me.netbirdUser ?? me.fqdn)
        } else if model.connectedAddress != nil {
            Text("Not connected")
        } else {
            Text("No print server set up")
        }
        if let error = model.error {
            Text(error).lineLimit(2)
        }
        let waiting = model.jobs.filter { !$0.failed }.count
        if waiting > 0 {
            Text("\(waiting) job(s) waiting")
        }
        Divider()
        Button("Open Platen…") { show(openWindow) }
        Button("Sync Printers Now") { Task { await model.refresh() } }
            .disabled(!model.configured || model.busy)
        Divider()
        Button("Quit Platen") { NSApp.terminate(nil) }
    }
}

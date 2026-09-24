import PortkeeperKit
import SwiftUI
import WebKit

/// The console window: the daemon's own page in a WKWebView. It is the same page a
/// browser tab would show, same origin as the daemon, so the daemon's origin guard sees
/// the same requests it always has.
struct ConsoleView: View {
    @ObservedObject var route: ConsoleRoute

    var body: some View {
        ConsoleWebView(url: route.url, token: route.token)
            .frame(minWidth: 720, minHeight: 480)
    }
}

struct ConsoleWebView: NSViewRepresentable {
    let url: URL
    let token: Int

    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
        var loadedToken = -1

        /// The console's links to a mapping (http://127.0.0.1:<port>/) belong in the
        /// operator's browser, not inside this window.
        func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            if let target = action.request.url, !isConsole(target), action.navigationType == .linkActivated {
                NSWorkspace.shared.open(target)
                decisionHandler(.cancel)
                return
            }
            decisionHandler(.allow)
        }

        /// target=_blank and window.open land here; send them to the browser too.
        func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                     for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
            if let target = action.request.url { NSWorkspace.shared.open(target) }
            return nil
        }

        private func isConsole(_ u: URL) -> Bool {
            u.host == DaemonClient.base.host && u.port == DaemonClient.base.port
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        let view = WKWebView(frame: .zero, configuration: config)
        view.navigationDelegate = context.coordinator
        view.uiDelegate = context.coordinator
        return view
    }

    func updateNSView(_ view: WKWebView, context: Context) {
        guard context.coordinator.loadedToken != token else { return }
        context.coordinator.loadedToken = token
        // "/" to "/#add" on a loaded page is a fragment navigation: the page gets a
        // hashchange, not a fresh load. Asking for the URL already showing reloads it, so
        // a second "Add" still opens the sheet.
        if view.url == url {
            view.reload()
        } else {
            view.load(URLRequest(url: url))
        }
    }
}

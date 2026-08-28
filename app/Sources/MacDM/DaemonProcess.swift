import Foundation

/// Wire version this app expects from macdmd. Must match internal/config.Version.
/// On mismatch the app restarts the daemon so a stale background macdmd left
/// over from a previous build never lingers.
let MacDMExpectedVersion = "0.4.6"

/// Starts and supervises a local `macdmd`. In a shipped app this would be a
/// LaunchAgent; spawning a child keeps the dev loop simple.
final class DaemonProcess {
    private var process: Process?

    static func locate() -> String? {
        let exeDir = (Bundle.main.executablePath as NSString?)?.deletingLastPathComponent
        var candidates: [String] = []
        if let d = exeDir {
            candidates.append(d + "/macdmd")
            candidates.append(d + "/../../../bin/macdmd") // .build/MacDM -> repo/bin
            candidates.append(d + "/../../../../bin/macdmd")
        }
        candidates.append(FileManager.default.currentDirectoryPath + "/bin/macdmd")
        candidates.append((NSHomeDirectory() as NSString).appendingPathComponent("bin/macdmd"))
        for c in candidates where FileManager.default.isExecutableFile(atPath: c) {
            return (c as NSString).standardizingPath
        }
        return nil
    }

    func ensureRunning(addr: String, completion: @escaping (Bool) -> Void) {
        health(addr: addr) { info in
            switch info {
            case .running(let version) where version == MacDMExpectedVersion:
                completion(true)
            case .running:
                NSLog("MacDM: restarting stale daemon")
                self.shutdown(addr: addr) {
                    self.spawn(addr: addr, completion: completion)
                }
            case .down:
                self.spawn(addr: addr, completion: completion)
            }
        }
    }

    private func spawn(addr: String, completion: @escaping (Bool) -> Void) {
        guard let bin = Self.locate() else {
            NSLog("MacDM: macdmd not found — run it manually: bin/macdm daemon")
            completion(false)
            return
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: bin)
        p.arguments = ["-addr", addr]
        do {
            try p.run()
            process = p
        } catch {
            NSLog("MacDM: failed to launch macdmd: \(error)")
            completion(false)
            return
        }
        // poll for it to bind (up to ~4s)
        var tries = 0
        func poll() {
            tries += 1
            health(addr: addr) { info in
                if case .running = info { completion(true) }
                else if tries < 20 { DispatchQueue.main.asyncAfter(deadline: .now() + 0.2, execute: poll) }
                else { completion(false) }
            }
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3, execute: poll)
    }

    func stop() { process?.terminate(); process = nil }

    // MARK: probes

    private enum HealthInfo { case running(version: String), down }

    private func health(addr: String, completion: @escaping (HealthInfo) -> Void) {
        guard let url = URL(string: "http://\(addr)/api/health") else { completion(.down); return }
        var req = URLRequest(url: url)
        req.timeoutInterval = 1.5
        URLSession.shared.dataTask(with: req) { data, resp, _ in
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            var version = ""
            if let data = data,
               let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let v = obj["version"] as? String {
                version = v
            }
            DispatchQueue.main.async {
                completion(code == 200 ? .running(version: version) : .down)
            }
        }.resume()
    }

    private func shutdown(addr: String, then: @escaping () -> Void) {
        guard let url = URL(string: "http://\(addr)/api/shutdown") else { then(); return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.timeoutInterval = 2
        URLSession.shared.dataTask(with: req) { _, _, _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.8, execute: then)
        }.resume()
    }
}

import Foundation

/// One connection's slice — the IDM "progress by connections" row.
struct ConnStat: Codable {
    var index: Int
    var start: Int64
    var downloaded: Int64
    var total: Int64
    var status: String
    var info: String?
}

/// A user-facing quality option.
struct FormatChoice: Codable, Hashable {
    var id: String
    var label: String
    var height: Int?
    var fps: Int?
    var ext: String?
    var size_bytes: Int64?
    var kind: String?
}

/// A job as reported by macdmd (field names match Go `store.Job` JSON tags).
struct Job: Codable, Identifiable {
    let id: String
    var kind: String
    var url: String
    var filename: String
    var dest: String?
    var status: String
    var error: String?
    var total_bytes: Int64
    var done_bytes: Int64
    var speed_bps: Int64
    var connections: Int
    var resumable: Bool?
    var quality: String?
    var conns: [ConnStat]?
    var segments: Int?
    var segments_done: Int?

    var percent: Double {
        total_bytes > 0 ? min(100, Double(done_bytes) / Double(total_bytes) * 100) : 0
    }
    var isRunning: Bool { status == "downloading" || status == "probing" || status == "queued" }

    /// An adaptive stream in flight: total/done bytes are estimates and the real
    /// segment counts are in `segments` / `segments_done`.
    var streamingSegments: Bool {
        (kind == "hls" || kind == "dash") && status != "completed" && (segments ?? 0) > 0
    }
    var sizeText: String {
        if streamingSegments { return "\(segments ?? 0) segments" }
        return total_bytes > 0 ? Fmt.bytes(total_bytes) : "—"
    }
    var etaText: String {
        guard status == "downloading", total_bytes > 0, speed_bps > 0 else { return "—" }
        return Fmt.eta(done: done_bytes, total: total_bytes, bps: speed_bps)
    }
    var doneText: String {
        if streamingSegments {
            return "\(segments_done ?? 0) / \(segments ?? 0) segments  (\(String(format: "%.1f", percent))%)"
        }
        return total_bytes > 0
            ? "\(Fmt.bytes(done_bytes))  (\(String(format: "%.1f", percent))%)"
            : Fmt.bytes(done_bytes)
    }
}

/// A caught download awaiting the "New Download" dialog.
struct Proposal: Codable, Identifiable {
    let id: String
    var url: String
    var kind: String
    var category: String?
    var title: String?
    var filename: String
    var size: Int64
    var resumable: Bool
    var drm: Bool
    var probing: Bool?
    var formats: [FormatChoice]?
}

/// Result of POST /api/probe.
struct ProbeResult: Codable {
    var kind: String
    var url: String
    var title: String?
    var filename: String
    var size: Int64
    var resumable: Bool
    var drm: Bool
    var live: Bool
    var formats: [FormatChoice]?
    var note: String?
}

/// Result of GET /api/tools.
struct ToolsInfo: Codable {
    struct Tool: Codable { var path: String; var version: String }
    struct YtDlp: Codable {
        var path: String
        var version: String
        var latest: String
        var channel: String?
        var update_available: Bool
    }
    var ffmpeg: Tool
    var ytdlp: YtDlp
    var auto_update: Bool
    var cookies_from: String?
}

private struct UpdateResult: Codable { var ok: Bool; var from: String; var to: String }

private struct JobEnvelope: Codable { let type: String; let job: Job }
private struct ProposalEnvelope: Codable { let type: String; let proposal: Proposal }
private struct TypeOnly: Codable { let type: String }

/// Talks to the local daemon: REST commands + a live SSE stream that keeps the
/// job list and pending proposals in sync, reconnecting on drop.
final class DaemonClient: NSObject, URLSessionDataDelegate {
    static let shared = DaemonClient()

    private(set) var addr = "127.0.0.1:7345"
    private var base: URL { URL(string: "http://\(addr)")! }

    /// Multiple views subscribe to job updates (menu, main window, detail
    /// windows) — a single `var onChange` closure only lets the last one win.
    private var jobListeners: [([Job]) -> Void] = []
    func onJobs(_ h: @escaping ([Job]) -> Void) {
        jobListeners.append(h)
        h(snapshotJobs()) // prime with current
    }
    private func emitJobs() {
        let snap = snapshotJobs()
        DispatchQueue.main.async { self.jobListeners.forEach { $0(snap) } }
    }

    /// `jobs` is written on the URLSession delegate queue and read from the main
    /// thread (onJobs primes a new listener), so every access takes the lock —
    /// concurrent use of a Swift Dictionary is undefined behaviour, not just a
    /// stale read.
    private func snapshotJobs() -> [Job] {
        jobsLock.lock()
        defer { jobsLock.unlock() }
        return jobs.values.sorted { $0.filename < $1.filename }
    }

    var onConnection: ((Bool) -> Void)?
    /// Raised on the main thread when a new proposal needs the dialog.
    var onProposal: ((Proposal) -> Void)?

    private let jobsLock = NSLock()
    private var jobs: [String: Job] = [:] // guarded by jobsLock
    /// When the current SSE stream connected. The daemon replays every existing
    /// job (old completed ones included) right after connect; we suppress
    /// "download complete" notifications for a moment so that backlog is silent.
    private var streamConnectedAt = Date.distantPast
    private var sseTask: URLSessionDataTask?
    private lazy var session = URLSession(configuration: .default, delegate: self, delegateQueue: nil)
    private var buffer = Data()

    func configure(addr: String) { self.addr = addr }

    // MARK: Commands

    private func post(_ path: String, _ body: [String: Any], _ done: ((Data?, Int) -> Void)? = nil) {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = "POST"
        if !body.isEmpty {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        }
        URLSession.shared.dataTask(with: req) { data, resp, _ in
            done?(data, (resp as? HTTPURLResponse)?.statusCode ?? 0)
        }.resume()
    }

    func add(url: String, dest: String? = nil, conns: Int? = nil, formatID: String? = nil,
             quality: String? = nil, filename: String? = nil,
             completion: ((Result<Job, Error>) -> Void)? = nil) {
        var body: [String: Any] = ["url": url]
        if let n = filename, !n.isEmpty { body["filename"] = n }
        if let d = dest { body["dest"] = d }
        if let c = conns { body["conns"] = c }
        if let f = formatID { body["format_id"] = f }
        if let q = quality { body["quality"] = q }
        post("api/jobs", body) { data, code in
            // Hop to main like every other completion here: callers open windows
            // from this, and AppKit off the main thread is undefined behaviour.
            let result: Result<Job, Error>
            if let data = data {
                if code >= 300 {
                    let msg = (try? JSONDecoder().decode([String: String].self, from: data))?["error"] ?? "HTTP \(code)"
                    result = .failure(Err.server(msg))
                } else if let job = try? JSONDecoder().decode(Job.self, from: data) {
                    result = .success(job)
                } else {
                    result = .failure(Err.server("unreadable response"))
                }
            } else {
                result = .failure(Err.empty)
            }
            DispatchQueue.main.async { completion?(result) }
        }
    }

    func command(_ id: String, _ action: String) { post("api/jobs/\(id)/\(action)", [:]) }

    func setConns(_ id: String, _ n: Int) { post("api/jobs/\(id)/conns", ["conns": n]) }

    func remove(_ id: String) {
        var req = URLRequest(url: base.appendingPathComponent("api/jobs/\(id)"))
        req.httpMethod = "DELETE"
        URLSession.shared.dataTask(with: req).resume()
    }

    func probe(_ url: String, completion: @escaping (ProbeResult?) -> Void) {
        post("api/probe", ["url": url]) { data, _ in
            let r = data.flatMap { try? JSONDecoder().decode(ProbeResult.self, from: $0) }
            DispatchQueue.main.async { completion(r) }
        }
    }

    /// Accepts a proposal. The daemon replies 201 with the job it created —
    /// `completion` receives it so the caller can open the progress window
    /// straight away instead of waiting for it to turn up over SSE.
    func accept(_ proposalID: String, dest: String?, filename: String?, conns: Int,
                formatID: String?, quality: String?,
                completion: ((Job?) -> Void)? = nil) {
        var body: [String: Any] = ["conns": conns]
        if let d = dest { body["dest"] = d }
        if let f = filename { body["filename"] = f }
        if let fid = formatID { body["format_id"] = fid }
        if let q = quality { body["quality"] = q }
        post("api/proposals/\(proposalID)/accept", body) { data, code in
            let job = (code < 300) ? data.flatMap { try? JSONDecoder().decode(Job.self, from: $0) } : nil
            DispatchQueue.main.async { completion?(job) }
        }
    }

    func reject(_ proposalID: String) { post("api/proposals/\(proposalID)/reject", [:]) }

    // MARK: Tools / config

    func fetchTools(_ done: @escaping (ToolsInfo?) -> Void) {
        let req = URLRequest(url: base.appendingPathComponent("api/tools"))
        URLSession.shared.dataTask(with: req) { data, _, _ in
            let info = data.flatMap { try? JSONDecoder().decode(ToolsInfo.self, from: $0) }
            DispatchQueue.main.async { done(info) }
        }.resume()
    }

    /// Runs the yt-dlp self-update on the daemon. Completion carries the new
    /// version string on success, or a non-nil error message on failure.
    func updateYtDlp(_ done: @escaping (_ newVersion: String?, _ error: String?) -> Void) {
        var req = URLRequest(url: base.appendingPathComponent("api/tools/ytdlp/update"))
        req.httpMethod = "POST"
        req.timeoutInterval = 600 // ~35 MB download; slow links need room
        URLSession.shared.dataTask(with: req) { data, resp, err in
            DispatchQueue.main.async {
                if let err = err { done(nil, err.localizedDescription); return }
                let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
                guard let data = data else { done(nil, "no response"); return }
                if code >= 300 {
                    let msg = (try? JSONDecoder().decode([String: String].self, from: data))?["error"] ?? "HTTP \(code)"
                    done(nil, msg)
                    return
                }
                let r = try? JSONDecoder().decode(UpdateResult.self, from: data)
                done(r?.to ?? "", nil)
            }
        }.resume()
    }

    func setConfig(_ body: [String: Any]) { post("api/config", body) }

    // MARK: SSE

    func startStream() {
        stopStream()
        buffer.removeAll()
        var req = URLRequest(url: base.appendingPathComponent("api/events"))
        req.timeoutInterval = .infinity
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        sseTask = session.dataTask(with: req)
        sseTask?.resume()
    }

    func stopStream() { sseTask?.cancel(); sseTask = nil }

    func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        buffer.append(data)
        while let range = buffer.range(of: Data("\n\n".utf8)) {
            let chunk = buffer.subdata(in: buffer.startIndex..<range.lowerBound)
            buffer.removeSubrange(buffer.startIndex..<range.upperBound)
            for line in String(decoding: chunk, as: UTF8.self).split(separator: "\n") {
                guard line.hasPrefix("data: ") else { continue }
                handle(Data(line.dropFirst(6).utf8))
            }
        }
        self.emitJobs()
    }

    private func handle(_ json: Data) {
        guard let t = try? JSONDecoder().decode(TypeOnly.self, from: json) else { return }
        switch t.type {
        case "job":
            if let e = try? JSONDecoder().decode(JobEnvelope.self, from: json) {
                jobsLock.lock()
                let prev = jobs[e.job.id]
                jobs[e.job.id] = e.job
                jobsLock.unlock()
                if e.job.status == "completed", prev?.status != "completed",
                   Date().timeIntervalSince(streamConnectedAt) > 3 {
                    DispatchQueue.main.async { Notifier.downloadFinished(e.job) }
                }
            }
        case "delete":
            if let e = try? JSONDecoder().decode(JobEnvelope.self, from: json) {
                jobsLock.lock()
                jobs.removeValue(forKey: e.job.id)
                jobsLock.unlock()
            }
        case "proposal":
            if let e = try? JSONDecoder().decode(ProposalEnvelope.self, from: json) {
                DispatchQueue.main.async { self.onProposal?(e.proposal) }
            }
        default: break
        }
    }

    func urlSession(_ s: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        DispatchQueue.main.async { self.onConnection?(false) }
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in self?.startStream() }
    }

    func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse,
                    completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        if let h = response as? HTTPURLResponse, h.statusCode == 200 {
            streamConnectedAt = Date()
            DispatchQueue.main.async { self.onConnection?(true) }
        }
        completionHandler(.allow)
    }

    enum Err: LocalizedError {
        case empty, server(String)
        var errorDescription: String? {
            switch self { case .empty: return "empty response"; case .server(let m): return m }
        }
    }
}

// MARK: - formatting helpers (shared by the windows)

enum Fmt {
    static func bytes(_ n: Int64) -> String {
        if n <= 0 { return "0 B" }
        let u = ["B", "KB", "MB", "GB", "TB"]
        var v = Double(n), i = 0
        while v >= 1024 && i < u.count - 1 { v /= 1024; i += 1 }
        return String(format: i == 0 ? "%.0f %@" : "%.2f %@", v, u[i])
    }

    /// Middle-truncate a long string: "https://very…/long/tail".
    static func short(_ s: String, _ max: Int = 68) -> String {
        guard s.count > max else { return s }
        let head = max * 3 / 5, tail = max - head - 1
        return s.prefix(head) + "…" + s.suffix(tail)
    }
    static func speed(_ n: Int64) -> String { n > 0 ? bytes(n) + "/s" : "—" }
    static func eta(done: Int64, total: Int64, bps: Int64) -> String {
        guard bps > 0, total > done else { return "—" }
        let secs = Double(total - done) / Double(bps)
        if secs < 60 { return "\(Int(secs)) sec" }
        if secs < 3600 { return "\(Int(secs / 60)) min \(Int(secs.truncatingRemainder(dividingBy: 60))) sec" }
        return String(format: "%.1f hr", secs / 3600)
    }
}

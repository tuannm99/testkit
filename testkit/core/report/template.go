package report

// reportTemplate is the offline report. Section order per test case follows
// the QC reading order: purpose/requirement/risk -> input -> steps + timeline
// -> assertions -> outputs -> Grafana -> why it passed -> counter-evidence.
const reportTemplate = `<!doctype html>
<html lang="vi">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TestKit report {{.Run.RunID}}</title>
<style>
:root{--fg:#1d2330;--muted:#5d6678;--line:#d9dde5;--bg:#fff;--soft:#f5f6f9;--ok:#16794c;--okbg:#e5f5ec;--bad:#b42318;--badbg:#fdecea;--err:#9a5b00;--errbg:#fff4de;--acc:#2f5bd3}
*{box-sizing:border-box}
body{margin:0;font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:var(--fg);background:var(--bg)}
main{max-width:1180px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:18px;margin:32px 0 8px;border-bottom:2px solid var(--line);padding-bottom:4px}
h3{font-size:15px;margin:20px 0 6px}h4{font-size:13px;margin:14px 0 4px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
.muted{color:var(--muted)}a{color:var(--acc)}
table{border-collapse:collapse;width:100%;margin:6px 0 10px}th,td{border:1px solid var(--line);padding:5px 8px;text-align:left;vertical-align:top}
th{background:var(--soft);font-weight:600}td.num{text-align:right;font-variant-numeric:tabular-nums}
code,pre{font:12px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
pre{background:var(--soft);border:1px solid var(--line);padding:8px;overflow:auto;max-height:360px;margin:4px 0}
.pill{display:inline-block;padding:1px 8px;border-radius:10px;font-weight:600;font-size:12px}
.ok{color:var(--ok);background:var(--okbg)}.bad{color:var(--bad);background:var(--badbg)}.err{color:var(--err);background:var(--errbg)}.muted.pill{background:var(--soft)}
.cards{display:flex;gap:10px;flex-wrap:wrap;margin:12px 0}.card{border:1px solid var(--line);border-radius:6px;padding:8px 14px;min-width:120px}
.card b{display:block;font-size:22px}
section.case{border:1px solid var(--line);border-radius:8px;padding:4px 16px 12px;margin:22px 0}
section.case>h3{font-size:17px}
.kv{display:grid;grid-template-columns:170px 1fr;gap:2px 12px}.kv div:nth-child(odd){color:var(--muted)}
figure{margin:8px 0;border:1px solid var(--line);border-radius:6px;padding:6px}figure img{max-width:100%;display:block}
figcaption{font-size:12px;color:var(--muted)}
ol.why li{margin:4px 0}.ev{font-size:12px}
details summary{cursor:pointer;color:var(--acc)}
.note{border-left:4px solid var(--err);background:var(--errbg);padding:6px 10px;margin:6px 0}
@media print{section.case{page-break-inside:avoid}pre{max-height:none}}
</style>
</head>
<body><main>
<h1>Báo cáo kiểm thử TestKit</h1>
<div class="muted">Run <code>{{.Run.RunID}}</code>{{if .Run.Suite}} · suite <code>{{.Run.Suite}}</code>{{end}} · {{date .Run.StartedAt}} → {{date .Run.FinishedAt}} · tạo lúc {{.Generated}}</div>

<div class="cards">
 <div class="card"><span class="muted">Pass</span><b class="pill ok">{{index .Counts "pass"}}</b></div>
 <div class="card"><span class="muted">Fail</span><b class="pill bad">{{index .Counts "fail"}}</b></div>
 <div class="card"><span class="muted">Error</span><b class="pill err">{{index .Counts "error"}}</b></div>
 <div class="card"><span class="muted">Skipped</span><b class="pill muted">{{index .Counts "skipped"}}</b></div>
 {{with .Run.Gate}}<div class="card"><span class="muted">Cổng release</span><b class="pill {{cls .Decision}}">{{.Decision}}</b></div>{{end}}
</div>

<p class="muted">Kết quả pass/fail do luật cứng quyết định (assertion có toán tử và giá trị kỳ vọng, ngưỡng SLO). Mỗi kết luận trỏ tới tệp bằng chứng trong thư mục run; tính toàn vẹn kiểm bằng <code>testkit verify out/{{.Run.RunID}}</code> (sha256 trong <code>manifest.json</code>).</p>
{{range .Run.Notes}}<div class="note">{{.}}</div>{{end}}

{{with .Run.Gate}}
<h2>Cổng release</h2>
<table><tr><th>Luật</th><th>Yêu cầu</th><th>Thực tế</th><th>Kết quả</th></tr>
{{range .Rules}}<tr><td>{{.Name}}</td><td>{{.Required}}</td><td>{{.Detail}}</td><td><span class="pill {{if .Passed}}ok{{else}}bad{{end}}">{{if .Passed}}đạt{{else}}không đạt{{end}}</span></td></tr>{{end}}
</table>
{{if .Flaky}}<h4>Danh sách cách ly (flaky)</h4><table><tr><th>Case</th><th>Lý do</th><th>Người xử lý</th><th>Hạn</th><th>Ticket</th></tr>{{range .Flaky}}<tr><td>{{.CaseID}}</td><td>{{.Reason}}</td><td>{{.Owner}}</td><td>{{.Deadline}}</td><td>{{.Ticket}}</td></tr>{{end}}</table>{{end}}
{{end}}

<h2>Tổng hợp</h2>
<table><tr><th>Testcase</th><th>Tiêu đề</th><th>Yêu cầu</th><th>Rủi ro</th><th>Trigger</th><th>Kết quả</th><th>Phản chứng</th><th>Thời gian</th></tr>
{{range .Execs}}<tr><td><a href="#{{.Anchor}}">{{.ID}}</a></td><td>{{.Title}}</td><td>{{join .Requirement ", "}}</td><td>{{.Risk}}</td><td>{{.Trigger}}</td>
<td><span class="pill {{cls .Result}}">{{upper .Result}}</span>{{if .Class}} <span class="muted">({{classVN .Class}})</span>{{end}}</td>
<td>{{if .Mutations}}{{range .Mutations}}<span class="pill {{if .Killed}}ok{{else}}bad{{end}}" title="{{.Title}}">{{.ID}} {{if .Killed}}đỏ ✓{{else}}sống sót ✗{{end}}</span> {{end}}{{else}}<span class="muted">—</span>{{end}}</td>
<td class="num">{{.Duration}}</td></tr>{{end}}
</table>

{{if .Run.Parity}}
<h2>So khớp giữa các trigger (cùng kịch bản, khác đường nhận job)</h2>
<table><tr><th>Testcase</th><th>Trigger</th><th>Khớp</th><th>Khác biệt (assertion: giá trị thực tế theo trigger)</th></tr>
{{range .Run.Parity}}<tr><td>{{.CaseID}}</td><td>{{join .Triggers ", "}}</td><td><span class="pill {{if .Match}}ok{{else}}bad{{end}}">{{if .Match}}khớp{{else}}khác{{end}}</span></td><td>{{range .Diffs}}{{.}}<br>{{end}}</td></tr>{{end}}
</table>
{{end}}

{{if .Run.Perf}}
<h2>Hiệu năng</h2>
<table><tr><th>Testcase</th><th>Loại</th><th>p50 / p95 / p99 (ms)</th><th>Thông lượng</th><th>Lỗi</th><th>Baseline</th><th>Kết quả</th></tr>
{{range .Run.Perf}}<tr><td><a href="#{{anchor .ID}}">{{.ID}}</a></td><td>{{.Kind}} ({{.Executor}}, ×{{.Repeat}})</td>
<td class="num">{{printf "%.0f" (index .Metrics "p50_ms")}} / {{printf "%.0f" (index .Metrics "p95_ms")}} / {{printf "%.0f" (index .Metrics "p99_ms")}}</td>
<td class="num">{{printf "%.2f" (index .Metrics "throughput")}}/s</td><td class="num">{{printf "%.2f%%" (pct (index .Metrics "error_rate"))}}</td>
<td>{{if .Comparisons}}{{range .Comparisons}}<span class="pill {{if .Regression}}bad{{else}}ok{{end}}" title="{{.Verdict}}">{{.Metric}} {{worse .ChangePct}}</span> {{end}}{{else}}<span class="muted">{{.Note}}</span>{{end}}</td>
<td><span class="pill {{cls .Result}}">{{.Result}}</span></td></tr>{{end}}
</table>
{{end}}
{{if .Run.Chaos}}
<h2>Chaos</h2>
<table><tr><th>Testcase</th><th>Lỗi gây ra</th><th>Dừng khẩn cấp</th><th>Phục hồi</th><th>Kết quả</th></tr>
{{range .Run.Chaos}}<tr><td><a href="#{{anchor .ID}}">{{.ID}}</a></td><td class="ev">{{range .Faults}}{{.}}<br>{{end}}</td><td>{{if .Aborted}}có{{else}}không{{end}}</td>
<td class="num">{{if ge .RecoveryS 0.0}}{{printf "%.1f" .RecoveryS}}s{{else}}—{{end}}{{if .MaxRecover}} / ≤ {{printf "%.0f" .MaxRecover}}s{{end}}</td><td><span class="pill {{cls .Result}}">{{.Result}}</span></td></tr>{{end}}
</table>
{{end}}

<h2>Ma trận truy vết (yêu cầu → testcase → kết quả)</h2>
<table><tr><th>Yêu cầu</th><th>Testcase</th></tr>
{{range .Matrix}}<tr><td>{{.Req}}</td><td>{{range .Execs}}<a href="#{{anchor .ID}}">{{.ID}}</a> <span class="pill {{cls .Result}}">{{.Result}}</span><br>{{end}}</td></tr>{{end}}
</table>
<p class="muted">Bản máy đọc: <a href="traceability.csv">traceability.csv</a> · <a href="junit.xml">junit.xml</a> · <a href="run.json">run.json</a> · <a href="manifest.json">manifest.json</a></p>

{{range .Execs}}{{$ex := .}}
<section class="case" id="{{.Anchor}}">
<h3>{{.ID}} — {{.Title}} <span class="pill {{cls .Result}}">{{upper .Result}}</span></h3>

<h4>1. Mục đích · yêu cầu · rủi ro</h4>
<div class="kv">
 <div>Mục đích</div><div>{{.Purpose}}</div>
 <div>Yêu cầu</div><div>{{join .Requirement ", "}}</div>
 <div>Rủi ro</div><div>{{.Risk}}</div>
 <div>Trạng thái testcase</div><div>{{.Status}}{{if .Owner}} · người duyệt {{.Owner}}{{end}}</div>
 <div>Service · trigger</div><div>{{.Service}}{{if .Trigger}} · {{.Trigger}}{{end}}</div>
 <div>Namespace</div><div><code>{{.NS}}</code></div>
 <div>Image</div><div><code>{{.Image}}</code> <span class="muted">{{.ImageID}}</span>{{if .Failpoints}} · failpoints <code>{{join .Failpoints "; "}}</code>{{end}}</div>
 <div>Tệp kịch bản</div><div><code>{{.File}}</code></div>
 <div>Tiền điều kiện</div><div>{{range .Preconditions}}• {{.}}<br>{{end}}</div>
</div>

<h4>2. Đầu vào</h4>
<pre>{{.InputJSON}}</pre>
{{if .VarsJSON}}<details><summary>Biến (vars) đã render cho namespace</summary><pre>{{.VarsJSON}}</pre></details>{{end}}

<h4>3. Các bước và timeline</h4>
<table><tr><th>#</th><th>Bước</th><th>Tham số</th><th>Kết quả</th><th>Bắt đầu</th><th>Thời lượng</th></tr>
{{range .Steps}}<tr><td class="num">{{.N}}</td><td><code>{{.Name}}</code>{{if .Label}}<div class="ev">{{.Label}}</div>{{end}}</td><td><code>{{compact .With}}</code>{{with .Output}}<details><summary>kết quả</summary><pre>{{compact .}}</pre></details>{{end}}</td>
<td><span class="pill {{cls .Result}}">{{.Result}}</span> {{.Error}}</td><td>{{since $ex.StartedAt .StartedAt}}</td><td>{{dur .StartedAt .FinishedAt}}</td></tr>{{end}}
</table>
<details><summary>Timeline đầy đủ ({{len .Timeline}} sự kiện, <a href="{{.Dir}}/timeline.json">timeline.json</a>)</summary>
<table><tr><th>Lúc</th><th>Loại</th><th>Sự kiện</th><th>Trạng thái</th><th>Thời lượng</th><th>Chi tiết</th></tr>
{{range .Timeline}}<tr><td>{{since $ex.StartedAt .At}}</td><td>{{.Kind}}</td><td>{{.Name}}</td><td><span class="pill {{cls .Status}}">{{.Status}}</span></td><td>{{dur .At .End}}</td><td>{{.Detail}}</td></tr>{{end}}
</table></details>

<h4>4. Assertion — kỳ vọng / thực tế / kết quả / lý do</h4>
<table><tr><th>ID</th><th>Kiểm tra</th><th>Kỳ vọng</th><th>Thực tế</th><th>Kết quả</th><th>Vì sao cần</th><th>Bằng chứng</th></tr>
{{range .Assertions}}<tr><td>{{.ID}}</td><td><code>{{.Check}}</code><div class="muted ev">{{.Source}}</div></td>
<td>{{op .Operator}} {{if needsExp .Operator}}<code>{{show .Expected}}</code>{{end}}</td>
<td><code>{{show .Actual}}</code><div class="muted ev">lúc {{t .ObservedAt}}, lần đo {{.Attempts}}</div></td>
<td><span class="pill {{cls .Result}}">{{.Result}}</span>{{if .Message}}<div class="ev">{{.Message}}</div>{{end}}</td>
<td>{{.Why}}</td><td class="ev">{{range .Evidence}}<a href="{{.}}">{{base .}}</a><br>{{end}}</td></tr>{{end}}
</table>

{{with .Perf}}{{$p := .}}
<h4>4b. Hiệu năng ({{.Kind}}, executor {{.Executor}}, {{.Repeat}} lần đo)</h4>
<table><tr><th>Chỉ số</th><th>Median</th><th>Từng lần đo</th><th>SLO</th></tr>
{{range $k, $v := .Metrics}}<tr><td>{{$k}}</td><td class="num">{{num $v}}</td><td class="ev">{{range index $p.Samples $k}}{{num .}} {{end}}</td><td>{{sloFor $p.Thresholds $k}}</td></tr>{{end}}
</table>
{{if .Comparisons}}<p>So với baseline <code>{{.BaselineKey}}</code> của môi trường <code>{{.BaselineEnv}}</code> (ghi lúc {{.BaselineAt}}), kiểm định Mann-Whitney U một phía, α = 0.05:</p>
<table><tr><th>Chỉ số</th><th>Baseline (median, mẫu)</th><th>Lần này (median, mẫu)</th><th>Mức thay đổi</th><th>p</th><th>Kết luận</th></tr>
{{range .Comparisons}}<tr><td>{{.Metric}}</td><td class="num">{{num .BaseMedian}}<div class="ev">{{range .Baseline}}{{num .}} {{end}}</div></td>
<td class="num">{{num .CurMedian}}<div class="ev">{{range .Current}}{{num .}} {{end}}</div></td><td class="num">{{worse .ChangePct}}</td><td class="num">{{printf "%.3f" .PValue}}</td>
<td><span class="pill {{if .Regression}}bad{{else}}ok{{end}}">{{if .Regression}}regression{{else}}không regression{{end}}</span> <span class="ev">{{.Verdict}}</span></td></tr>{{end}}
</table>{{end}}
{{if .Note}}<p class="muted">{{.Note}}</p>{{end}}
<p class="muted">Tải theo open model (tốc độ đến cố định), warm-up không tính vào phép đo; "late" = số job máy tạo tải phát trễ (máy tạo tải bị nghẽn). Số liệu thô: <a href="{{.Dir}}/summary.json">summary.json</a>.</p>
{{end}}
{{with .Chaos}}
<h4>4c. Thí nghiệm chaos</h4>
<div class="kv"><div>Lỗi gây ra</div><div>{{range .Faults}}<code>{{.}}</code><br>{{end}}</div>
<div>Dừng khẩn cấp</div><div>{{if .Aborted}}<span class="pill bad">có</span> {{.AbortWhy}}{{else}}không{{end}}</div>
<div>Phục hồi</div><div>{{if ge .RecoveryS 0.0}}{{printf "%.1f" .RecoveryS}}s{{else}}không đo được / chưa phục hồi{{end}}{{if .MaxRecover}} (cho phép ≤ {{printf "%.0f" .MaxRecover}}s){{end}}</div></div>
{{end}}

<h4>5. Đầu ra (response, snapshot DB, mail, journal mock, log)</h4>
{{if .Outputs}}<table><tr><th>Bằng chứng</th><th>Tệp</th></tr>
{{range .Outputs}}<tr><td>{{.Title}}{{if .Preview}}<details><summary>xem nhanh</summary><pre>{{.Preview}}</pre></details>{{end}}</td><td class="ev"><a href="{{.Path}}">{{.Path}}</a></td></tr>{{end}}
</table>{{else}}<p class="muted">Không có tệp đầu ra.</p>{{end}}

<h4>6. Grafana (ảnh để xem nhanh, số liệu thô là bằng chứng gốc)</h4>
{{if .Panels}}{{range .Panels}}<figure>
{{if .Image}}<img src="{{.Image}}" alt="{{.Title}}" loading="lazy">{{else}}<div class="note">Không render được ảnh: {{.Error}}</div>{{end}}
<figcaption><b>{{.Title}}</b> ({{.Name}}) · khoảng {{.Window}} · <a href="{{.Link}}">mở dashboard đã khoá thời gian</a> · dữ liệu thô: {{range .Raw}}<a href="{{.}}">{{base .}}</a> {{end}}{{range .CSV}}<a href="{{.}}">{{base .}}</a> {{end}}
{{if .Stats}}<br>{{range .Stats}}{{.}}<br>{{end}}{{end}}</figcaption></figure>{{end}}
{{else}}<p class="muted">Không có panel (evidence.grafana trống hoặc profile observability không chạy).</p>{{end}}

<h4>7. Kết luận — vì sao {{if eq .Result "pass"}}pass{{else}}không pass{{end}}</h4>
<ol class="why">{{range .Conclusion}}<li>{{.Text}}{{if .Evidence}} <span class="ev">Bằng chứng: {{range $i, $e := .Evidence}}<a href="{{$e}}">{{base $e}}</a> {{end}}</span>{{end}}</li>{{end}}</ol>
{{if ne .Result "pass"}}
<div class="kv"><div>Bước lệch</div><div>{{.FailedAt}}</div><div>Phân loại sơ bộ</div><div>{{classVN .Class}}</div><div>Lý do</div><div>{{.Reason}}</div></div>
{{if .LogTail}}<details open><summary>Log gần nhất của service</summary><pre>{{join .LogTail "\n"}}</pre></details>{{end}}
{{end}}

<h4>8. Bằng chứng phản chứng (mutation)</h4>
{{if .Mutations}}<table><tr><th>Mutation</th><th>Làm hỏng</th><th>Kết quả case khi hệ thống hỏng</th><th>Assertion đỏ</th><th>Kết luận</th></tr>
{{range .Mutations}}<tr><td>{{.ID}}</td><td>{{.Title}} <code>{{.Failpoint}}</code></td><td><span class="pill {{cls .Result}}">{{.Result}}</span> <a href="{{.Dir}}/case.json">case.json</a></td>
<td>{{join .RedIDs ", "}}{{if .Expected}} <span class="muted">(yêu cầu: {{join .Expected ", "}})</span>{{end}}</td>
<td><span class="pill {{if .Killed}}ok{{else}}bad{{end}}">{{if .Killed}}case đã đỏ — test có ý nghĩa{{else}}case KHÔNG đỏ{{end}}</span> {{.Detail}}</td></tr>{{end}}
</table>{{else}}<p class="muted">Chưa chạy mutation cho case này (<code>testkit run --mutations</code>).</p>{{end}}
</section>
{{end}}

{{with .Manifest}}
<h2>Môi trường và phiên bản</h2>
<div class="kv">
 <div>Commit</div><div><code>{{.Git.Commit}}</code> {{.Git.Branch}}{{if .Git.Dirty}} <span class="pill err">có thay đổi chưa commit</span>{{end}}</div>
 <div>Công cụ</div><div>{{.Tool}} {{.ToolVersion}}</div>
 <div>Máy</div><div>{{.Host.OS}}/{{.Host.Arch}}, {{.Host.CPUs}} CPU, docker {{.Host.DockerVersion}}</div>
 <div>Lệnh</div><div><code>{{join .Command " "}}</code></div>
</div>
<table><tr><th>Thành phần</th><th>Image đang chạy</th></tr>{{range $k, $v := .Running}}<tr><td>{{$k}}</td><td><code>{{$v}}</code></td></tr>{{end}}
{{range $k, $v := .Services}}<tr><td>service under test: {{$k}}</td><td><code>{{$v}}</code></td></tr>{{end}}</table>
{{if .Mocks}}<h4>Nguồn gốc mock</h4><table><tr><th>Service</th><th>Mock</th><th>Loại</th><th>Phiên bản API</th><th>Kiểm chứng ngày</th><th>Spec (sha256)</th></tr>
{{range .Mocks}}<tr><td>{{.Service}}</td><td>{{.Mock}}</td><td>{{.Kind}}</td><td>{{.APIVersion}}</td><td>{{.VerifiedAt}}</td><td><code>{{.Spec}}</code> <span class="muted">{{.SpecSHA256}}</span></td></tr>{{end}}</table>{{end}}
{{end}}
</main></body></html>`

"""Minimal stand-in for the Zephyr Scale API: records one upload and answers like the real one."""
import email.parser, email.policy, http.server, io, json, sys, threading, zipfile

port, out = int(sys.argv[1]), sys.argv[2]


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        msg = email.parser.BytesParser(policy=email.policy.HTTP).parsebytes(
            b"Content-Type: " + self.headers["Content-Type"].encode() + b"\r\n\r\n" + body)
        parts = {p.get_param("name", header="content-disposition"): p.get_payload(decode=True) for p in msg.iter_parts()}
        z = zipfile.ZipFile(io.BytesIO(parts["file"]))
        rec = {"path": self.path, "auth": self.headers.get("Authorization", "")[:7],
               "executions": json.loads(z.read(z.namelist()[0])), "testCycle": json.loads(parts["testCycle"])}
        with open(out, "w") as f:
            json.dump(rec, f, ensure_ascii=False, indent=1)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"testCycle":{"id":1,"key":"ORD-R1"}}')
        threading.Thread(target=srv.shutdown).start()

    def log_message(self, *a):
        pass


srv = http.server.HTTPServer(("127.0.0.1", port), H)
srv.serve_forever()

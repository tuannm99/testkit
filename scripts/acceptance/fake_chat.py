"""Stand-in for an OpenAI-compatible /chat/completions endpoint: records the request, answers a canned text."""
import http.server, json, sys, threading

port, record, answer_file = int(sys.argv[1]), sys.argv[2], sys.argv[3]


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with open(record, "w") as f:
            json.dump({"path": self.path, "auth": self.headers.get("Authorization", "")[:7], "body": body}, f, ensure_ascii=False)
        text = open(answer_file).read()
        out = {"model": body["model"], "choices": [{"message": {"content": text}, "finish_reason": "stop"}],
               "usage": {"prompt_tokens": 1, "completion_tokens": 1}}
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(out).encode())
        threading.Thread(target=srv.shutdown).start()

    def log_message(self, *a):
        pass


srv = http.server.HTTPServer(("127.0.0.1", port), H)
srv.serve_forever()

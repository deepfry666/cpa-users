"""Minimal OpenAI-compatible upstream for end-to-end tests."""
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = ["gpt-5.6-sol", "deepseek-v4-flash", "claude-opus-x"]

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _json(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
    def do_GET(self):
        if self.path.endswith("/models"):
            return self._json(200, {"object": "list", "data": [{"id": m, "object": "model", "owned_by": "mock"} for m in MODELS]})
        self._json(404, {"error": "nf"})
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        model = body.get("model", "?")
        time.sleep(float(body.get("metadata", {}).get("sleep", 0)) if isinstance(body.get("metadata"), dict) else 0)
        usage = {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150}
        if body.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            for piece in ["Hello", " from", " mock"]:
                chunk = {"id": "c1", "object": "chat.completion.chunk", "created": int(time.time()), "model": model,
                         "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]}
                self.wfile.write(f"data: {json.dumps(chunk)}\n\n".encode()); self.wfile.flush(); time.sleep(0.05)
            last = {"id": "c1", "object": "chat.completion.chunk", "created": int(time.time()), "model": model,
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": usage}
            self.wfile.write(f"data: {json.dumps(last)}\n\ndata: [DONE]\n\n".encode()); self.wfile.flush()
            return
        self._json(200, {"id": "c1", "object": "chat.completion", "created": int(time.time()), "model": model,
                         "choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello from mock"}, "finish_reason": "stop"}],
                         "usage": usage})

ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()

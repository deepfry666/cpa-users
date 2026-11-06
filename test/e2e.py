"""End-to-end checks against a running CPA with the cpa-users plugin."""
import json, sys, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor

BASE = "http://127.0.0.1:18417"
MGMT = BASE + "/v0/management/cpa-users/"
ADMIN = {"Authorization": "Bearer mgmt-test-secret"}
NATIVE = "sk-admin-native-test"
results = []

def req(method, url, headers=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method, headers={**(headers or {}), **({"Content-Type": "application/json"} if data else {})})
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw = resp.read(); return resp.status, raw, dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)

def j(raw):
    try: return json.loads(raw)
    except Exception: return raw.decode(errors="replace")

def check(name, cond, detail=""):
    results.append((name, bool(cond)))
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f"  -> {detail}"))

def mgmt(method, path, body=None):
    s, raw, _ = req(method, MGMT + path, ADMIN, body); return s, j(raw)

def chat(key, model, stream=False, header="bearer"):
    h = {"Authorization": f"Bearer {key}"} if header == "bearer" else {"x-api-key": key, "anthropic-version": "2023-06-01"}
    body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "stream": stream}
    return req("POST", BASE + "/v1/chat/completions", h, body)

# --- setup ---
s, g = mgmt("POST", "group/save", {"name": "基础", "models": ["gpt-5*", "deepseek-v4-flash"]})
check("create group", s == 200, g)
s, g2 = mgmt("POST", "group/save", {"name": "全部", "models": ["*"]})
s, ua = mgmt("POST", "user/save", {"name": "A", "group_id": g["id"], "daily_requests": 3, "max_concurrency": 1})
check("create user A", s == 200, ua)
s, ub = mgmt("POST", "user/save", {"name": "B", "group_id": g2["id"], "monthly_tokens": 1000000})
s, ka = mgmt("POST", "key/create", {"user_id": ua["id"], "name": "A-laptop"})
s, kb = mgmt("POST", "key/create", {"user_id": ub["id"], "name": "B-claude-code"})
KA, KB = ka["key"], kb["key"]
check("issue keys", KA.startswith("sk-cu-") and KB.startswith("sk-cu-"), (ka, kb))

# --- model list filtering ---
s, raw, _ = req("GET", BASE + "/v1/models", {"Authorization": f"Bearer {KA}"})
ids = sorted(m["id"] for m in j(raw).get("data", []))
check("A sees only allowed models in /v1/models", s == 200 and ids == ["deepseek-v4-flash", "gpt-5.6-sol"], (s, ids))
s, raw, _ = req("GET", BASE + "/v1/models", {"Authorization": f"Bearer {KB}"})
check("B (*) sees all models", s == 200 and len(j(raw).get("data", [])) == 3, j(raw))
s, raw, _ = req("GET", BASE + "/v1/models", {"Authorization": f"Bearer {NATIVE}"})
check("native admin key sees all models", s == 200 and len(j(raw).get("data", [])) == 3, j(raw))
s, raw, _ = req("GET", BASE + "/v1/models", {"x-api-key": KA, "anthropic-version": "2023-06-01"})
claude_ids = sorted(m.get("id") for m in j(raw).get("data", [])) if s == 200 else []
check("A Claude-format model list is filtered", s == 200 and "claude-opus-x" not in claude_ids and len(claude_ids) == 2, (s, j(raw)))
s, raw, _ = req("GET", BASE + "/v1beta/models", {"x-goog-api-key": KA})
gem = [m.get("name", "").replace("models/", "") for m in j(raw).get("models", [])] if s == 200 else []
check("A Gemini-format model list is filtered", s == 200 and "claude-opus-x" not in gem and len(gem) == 2, (s, j(raw)))
s, raw, _ = req("GET", BASE + "/v1/models/claude-opus-x", {"Authorization": f"Bearer {KA}"})
check("A model detail for hidden model is 404", s == 404, (s, raw[:200]))
s, raw, _ = req("GET", BASE + "/v1/models", {"Authorization": "Bearer sk-cu-doesnotexist"})
check("unknown plugin-style key is 401", s == 401, (s, raw[:200]))

# --- model gate ---
s, raw, _ = chat(KA, "gpt-5.6-sol")
check("A allowed model -> 200", s == 200, (s, raw[:300]))
s, raw, _ = chat(KA, "claude-opus-x")
body = j(raw)
check("A disallowed model -> 403 openai error", s == 403 and "无权使用模型" in json.dumps(body, ensure_ascii=False), (s, body))
s, raw, _ = req("POST", BASE + "/v1/messages", {"x-api-key": KA, "anthropic-version": "2023-06-01"},
                {"model": "claude-opus-x", "max_tokens": 10, "messages": [{"role": "user", "content": "hi"}]})
body = j(raw)
check("A disallowed model via /v1/messages -> 403 claude-shaped", s == 403 and isinstance(body, dict) and body.get("type") == "error", (s, body))
s, raw, _ = chat(KA, "gpt-5.6-sol", stream=True)
check("A streaming request -> 200 SSE", s == 200 and b"data:" in raw, (s, raw[:200]))
s, raw, _ = chat(NATIVE, "claude-opus-x")
check("native key is not gated", s == 200, (s, raw[:200]))

time.sleep(1.5)  # usage callbacks are asynchronous
s, st = mgmt("GET", "state")
a = next(u for u in st["users"] if u["name"] == "A")
check("A usage counted (2 successful requests)", a["quota"]["day"]["requests"] == 2, a["quota"])
check("A tokens counted (2 x 150)", a["quota"]["day"]["tokens"] == 300, a["quota"])

# --- quota ---
s, raw, _ = chat(KA, "deepseek-v4-flash")
check("A 3rd request -> 200", s == 200, (s, raw[:200]))
time.sleep(1.5)
s, raw, hdr = chat(KA, "gpt-5.6-sol")
check("A 4th request -> 429 quota with Retry-After", s == 429 and "今日请求次数已用完" in raw.decode() and "Retry-After" in hdr, (s, raw[:300], hdr))

# --- concurrency (user B unlimited requests, set max_concurrency 1 and fire 3 parallel slow requests) ---
mgmt("POST", "user/save", {**{k: v for k, v in next(u for u in st["users"] if u["name"] == "B").items() if k in ("id","name","note","group_id","extra_models","disabled","expires_at","daily_tokens","monthly_tokens","daily_requests","monthly_requests")}, "max_concurrency": 1})
def slow(_):
    return req("POST", BASE + "/v1/chat/completions", {"Authorization": f"Bearer {KB}"},
               {"model": "gpt-5.6-sol", "messages": [{"role": "user", "content": "hi"}], "metadata": {"sleep": 1.0}})[0]
with ThreadPoolExecutor(3) as ex:
    codes = sorted(ex.map(slow, range(3)))
check("B max_concurrency=1 -> one 200, others 429", codes.count(200) >= 1 and codes.count(429) >= 1, codes)
time.sleep(1.5)
s, raw, _ = chat(KB, "gpt-5.6-sol")
check("B slot released after completion", s == 200, (s, raw[:200]))

# --- reporting ---
time.sleep(1.5)
s, rep = mgmt("GET", "usage?days=7")
check("usage report has per-user totals", s == 200 and str(ua["id"]) in rep["by_user"] and str(ub["id"]) in rep["by_user"], rep.get("by_user"))
check("native traffic recorded as user 0", "0" in rep["by_user"], rep.get("by_user"))
s, lg = mgmt("GET", "logs?limit=50")
check("request logs present", s == 200 and len(lg["logs"]) >= 5, len(lg.get("logs", [])))
s, mr = mgmt("POST", "models/refresh", {})
check("models/refresh via host.auth.models", s == 200 and "claude-opus-x" in mr["models"], (s, mr))

# --- self-service ---
s, raw, _ = req("GET", BASE + "/v0/resource/plugins/cpa-users/me/data", {"Authorization": f"Bearer {KA}"})
me = j(raw)
check("me/data with user key", s == 200 and me["user"]["name"] == "A" and sorted(me["available"]) == ["deepseek-v4-flash", "gpt-5.6-sol"], (s, me if s != 200 else me["available"]))
s, raw, _ = req("GET", BASE + "/v0/resource/plugins/cpa-users/me/data", {"Authorization": "Bearer nope"})
check("me/data rejects bad key", s == 401, s)
s, raw, _ = req("GET", BASE + "/v0/resource/plugins/cpa-users/ui")
check("admin UI resource served", s == 200 and b"<title>" in raw, s)
s, raw, _ = req("GET", MGMT + "state", {"Authorization": "Bearer wrong"})
check("management API requires management key", s == 401, s)

# --- disable ---
mgmt("POST", "key/update", {"id": ka["id"], "name": "A-laptop", "disabled": True})
s, raw, _ = chat(KA, "gpt-5.6-sol")
check("disabled key -> 401", s == 401 and "已停用" in raw.decode(), (s, raw[:200]))

failed = [n for n, ok in results if not ok]
print(f"\n{len(results) - len(failed)}/{len(results)} passed")
sys.exit(1 if failed else 0)

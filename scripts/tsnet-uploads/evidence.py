#!/usr/bin/env python3
"""Evidence, from a production binary, that the embedded Tailscale node
uploads its diagnostic logs only where bridge.yaml allows it (backlog B749).

The repository's own test cases cannot see an upload: upstream's logger is
never started inside a test binary. This script runs a bridge binary as an
operator would, in tsnet mode, and counts what it asks a local HTTPS proxy to
reach, hermetically:

- Every run sends its HTTP and HTTPS through a proxy on 127.0.0.1 that logs
  each request's target (a CONNECT's host and port) and refuses every one. It
  never connects anywhere.
- The bridge runs in a sandbox it cannot leave: Landlock (Linux, ABI 4 or
  later) lets it connect a TCP socket to the proxy's port and to no other, and
  a seccomp filter refuses it any IPv4 or IPv6 socket that is not TCP (UDP,
  ICMP, raw), so neither a name lookup nor a STUN probe nor a direct dial can
  leave the host. TS_DEBUG_ALWAYS_USE_DERP=1, an upstream debug knob, keeps
  the node from binding UDP, which the sandbox refuses, so the node starts as
  it would on a host whose UDP is blocked. GODEBUG=netdns=go keeps every name
  lookup in Go's own resolver.
- Before any run the sandbox is checked from inside: a connect to the proxy's
  port succeeds, a connect to another port on 127.0.0.1 and every non-TCP
  IPv4 and IPv6 socket are refused. Nothing in the check leaves the host.

Pass and fail rest on the CONNECTs to log.tailscale.com, Tailscale's log
host (logtail.DefaultHost), alone: the coordination server's CONNECTs are
expected in every run. With tailscale.uploadDiagnosticLogs: true at least one
must come within the window; with the key absent, and with the key true and
TS_NO_LOGS_NO_SUPPORT=true in the bridge's environment, none may come over the
same window, which must be long beside the time the first one took.

Each run's bridge has a throwaway home, config and data dir under --work.
The node also says the name it registers with; a new node, and the second
start of a node whose first start registered nothing, must say 1-bit-bridge,
never the library's name.

usage:
  python3 -I evidence.py --bridge PATH --work DIR [--window S] [--rounds N]

Linux only. Exit status 0 when every expectation held. The result is printed
as a table and saved as JSON in the work dir.
"""

import argparse
import ctypes
import json
import os
import platform
import re
import signal
import socket
import socketserver
import subprocess
import sys
import threading
import time

LOG_HOST = "log.tailscale.com:443"
LIBRARY_NAME = "B749 Evidence Library"
DEFAULT_NAME = "1-bit-bridge"

# --- the sandbox, applied in a child just before it executes the bridge ---

PR_SET_NO_NEW_PRIVS = 38
PR_SET_SECCOMP = 22
SECCOMP_MODE_FILTER = 2
SECCOMP_RET_ALLOW = 0x7FFF0000
SECCOMP_RET_ERRNO = 0x00050000
SECCOMP_RET_KILL_PROCESS = 0x80000000
EPERM = 1
AF_INET, AF_INET6, SOCK_STREAM, IPPROTO_TCP = 2, 10, 1, 6
LANDLOCK_CREATE_RULESET_VERSION = 1
LANDLOCK_ACCESS_NET_CONNECT_TCP = 1 << 1
LANDLOCK_RULE_NET_PORT = 2

ARCH = {
    # machine: audit arch, socket, io_uring_setup, landlock create/add/restrict
    "x86_64": (0xC000003E, 41, 425, 444, 445, 446),
    "aarch64": (0xC00000B7, 198, 425, 444, 445, 446),
}


class SockFilter(ctypes.Structure):
    _fields_ = [("code", ctypes.c_uint16), ("jt", ctypes.c_uint8), ("jf", ctypes.c_uint8), ("k", ctypes.c_uint32)]


class SockFprog(ctypes.Structure):
    _fields_ = [("len", ctypes.c_ushort), ("filter", ctypes.POINTER(SockFilter))]


class RulesetAttr(ctypes.Structure):
    _fields_ = [("handled_access_fs", ctypes.c_uint64), ("handled_access_net", ctypes.c_uint64),
                ("scoped", ctypes.c_uint64)]


class NetPortAttr(ctypes.Structure):
    _fields_ = [("allowed_access", ctypes.c_uint64), ("port", ctypes.c_uint64)]


def assemble(prog):
    """Classic BPF from (label, code, jump-if-true, jump-if-false, k) rows
    whose jumps name labels; a jump is relative to the next row, forward."""
    at = {row[0]: i for i, row in enumerate(prog) if row[0]}
    out = []
    for i, (_, code, jt, jf, k) in enumerate(prog):
        rel = [0 if j is None else at[j] - (i + 1) for j in (jt, jf)]
        if min(rel) < 0:
            raise ValueError("a BPF jump goes backward")
        out.append((code, rel[0], rel[1], k))
    return out


def seccomp_program(arch, nr_socket, nr_io_uring):
    """A filter that refuses every IPv4 and IPv6 socket that is not plain TCP
    (protocol 0 or IPPROTO_TCP: UDP, ICMP, raw, SCTP and MPTCP are refused, and
    Go falls back to plain TCP from MPTCP), refuses io_uring, and kills a
    process calling in through another ABI."""
    LD_W_ABS, JEQ, JGE, AND, RET = 0x20, 0x15, 0x35, 0x54, 0x06
    return assemble([
        (None, LD_W_ABS, None, None, 4),                       # seccomp_data.arch
        (None, JEQ, "nr", "kill", arch),
        ("kill", RET, None, None, SECCOMP_RET_KILL_PROCESS),
        ("nr", LD_W_ABS, None, None, 0),                       # seccomp_data.nr
        (None, JGE, "deny", "uring", 0x40000000),              # an x32 call
        ("uring", JEQ, "deny", "sock", nr_io_uring),
        ("sock", JEQ, "domain", "allow", nr_socket),
        ("domain", LD_W_ABS, None, None, 16),                  # args[0], the domain
        (None, JEQ, "type", "inet6", AF_INET),
        ("inet6", JEQ, "type", "allow", AF_INET6),
        ("type", LD_W_ABS, None, None, 24),                    # args[1], the type
        (None, AND, None, None, 0xF),
        (None, JEQ, "proto", "deny", SOCK_STREAM),
        ("proto", LD_W_ABS, None, None, 32),                   # args[2], the protocol
        (None, JEQ, "allow", "tcp", 0),
        ("tcp", JEQ, "allow", "deny", IPPROTO_TCP),
        ("allow", RET, None, None, SECCOMP_RET_ALLOW),
        ("deny", RET, None, None, SECCOMP_RET_ERRNO | EPERM),
    ])


def apply_sandbox(port):
    """Confine this process, and what it executes, to TCP connects to port and
    to no non-TCP IPv4 or IPv6 socket. Raises where the kernel cannot."""
    machine = platform.machine()
    if sys.platform != "linux" or machine not in ARCH:
        raise SystemExit("the sandbox needs Linux on x86_64 or aarch64")
    arch, nr_socket, nr_io_uring, nr_create, nr_add, nr_restrict = ARCH[machine]
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    if libc.prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "prctl(PR_SET_NO_NEW_PRIVS)")
    abi = libc.syscall(nr_create, None, ctypes.c_size_t(0), ctypes.c_uint32(LANDLOCK_CREATE_RULESET_VERSION))
    if abi < 4:
        raise SystemExit(f"Landlock ABI {abi} has no network rules (4 or later is needed)")
    attr = RulesetAttr(0, LANDLOCK_ACCESS_NET_CONNECT_TCP, 0)
    size = ctypes.sizeof(RulesetAttr) if abi >= 6 else 16
    fd = libc.syscall(nr_create, ctypes.byref(attr), ctypes.c_size_t(size), ctypes.c_uint32(0))
    if fd < 0:
        raise OSError(ctypes.get_errno(), "landlock_create_ruleset")
    rule = NetPortAttr(LANDLOCK_ACCESS_NET_CONNECT_TCP, port)
    if libc.syscall(nr_add, ctypes.c_int(fd), ctypes.c_int(LANDLOCK_RULE_NET_PORT), ctypes.byref(rule),
                    ctypes.c_uint32(0)) != 0:
        raise OSError(ctypes.get_errno(), "landlock_add_rule")
    if libc.syscall(nr_restrict, ctypes.c_int(fd), ctypes.c_uint32(0)) != 0:
        raise OSError(ctypes.get_errno(), "landlock_restrict_self")
    os.close(fd)
    prog = seccomp_program(arch, nr_socket, nr_io_uring)
    filters = (SockFilter * len(prog))(*[SockFilter(*ins) for ins in prog])
    fprog = SockFprog(len(prog), ctypes.cast(filters, ctypes.POINTER(SockFilter)))
    if libc.prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, ctypes.byref(fprog), 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "prctl(PR_SET_SECCOMP)")


def sandbox_main(argv):
    """`evidence.py sandbox PORT -- PROGRAM ARGS...`: confine, then execute."""
    port = int(argv[0])
    if argv[1] != "--":
        raise SystemExit("usage: evidence.py sandbox PORT -- PROGRAM ARGS...")
    apply_sandbox(port)
    os.execv(argv[2], argv[2:])


# --- the proxy that logs every target and refuses every one ---

class Proxy(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self):
        self.lock = threading.Lock()
        self.seen = []  # (monotonic time, method, target)
        super().__init__(("127.0.0.1", 0), ProxyHandler)

    @property
    def port(self):
        return self.server_address[1]

    def note(self, method, target):
        with self.lock:
            self.seen.append((time.monotonic(), method, target))

    def since(self, t0):
        with self.lock:
            return [(t - t0, m, tgt) for (t, m, tgt) in self.seen if t >= t0]


class ProxyHandler(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(10)
        head = b""
        try:
            while b"\r\n\r\n" not in head and len(head) < 65536:
                chunk = self.request.recv(4096)
                if not chunk:
                    break
                head += chunk
        except OSError:
            pass
        line = head.split(b"\r\n", 1)[0].decode("latin-1")
        parts = line.split(" ")
        if len(parts) >= 2:
            method, target = parts[0], parts[1]
            if method != "CONNECT":
                m = re.match(r"^[a-z]+://([^/]+)", target)
                target = m.group(1) if m else target
            self.server.note(method, target)
            try:
                self.request.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            except OSError:
                pass


def serve_proxy():
    p = Proxy()
    threading.Thread(target=p.serve_forever, daemon=True).start()
    return p


# --- the sandbox's own check, from inside it, touching nothing off the host ---

PROBE = r"""
import json, socket, sys
port, other = int(sys.argv[1]), int(sys.argv[2])
out = {}
def tcp(p):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect(("127.0.0.1", p)); return "connected"
    except OSError as e:
        return "refused: errno %d" % e.errno
    finally:
        s.close()
out["tcp to the proxy's port"] = tcp(port)
out["tcp to another port on 127.0.0.1"] = tcp(other)
for name, args in (("udp4", (socket.AF_INET, socket.SOCK_DGRAM)), ("udp6", (socket.AF_INET6, socket.SOCK_DGRAM)),
                   ("icmp4", (socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP)),
                   ("raw4", (socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)),
                   ("sctp4", (socket.AF_INET, socket.SOCK_STREAM, 132)),
                   ("mptcp4", (socket.AF_INET, socket.SOCK_STREAM, 262)),
                   ("tcp6", (socket.AF_INET6, socket.SOCK_STREAM)),
                   ("unix", (socket.AF_UNIX, socket.SOCK_DGRAM))):
    try:
        socket.socket(*args).close(); out[name + " socket"] = "created"
    except OSError as e:
        out[name + " socket"] = "refused: errno %d" % e.errno
print(json.dumps(out))
"""


def check_sandbox(proxy):
    other = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    other.bind(("127.0.0.1", 0))
    other.listen(1)
    try:
        cmd = [sys.executable, "-I", os.path.abspath(__file__), "sandbox", str(proxy.port), "--",
               sys.executable, "-I", "-c", PROBE, str(proxy.port), str(other.getsockname()[1])]
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    finally:
        other.close()
    if out.returncode != 0:
        raise SystemExit("the sandbox check did not run: " + out.stderr)
    got = json.loads(out.stdout)
    want_ok = {"tcp to the proxy's port": "connected", "unix socket": "created", "tcp6 socket": "created"}
    ok = all(got.get(k) == v for k, v in want_ok.items())
    ok = ok and all(v.startswith("refused") for k, v in got.items() if k not in want_ok)
    return ok, got


# --- the runs ---

def write_install(work, name, key, inherited_off):
    root = os.path.join(work, name)
    for d in ("home", "music", "data"):
        os.makedirs(os.path.join(root, d), exist_ok=True)
    cfg = os.path.join(root, "bridge.yaml")
    body = (
        "libraryRoots:\n  - " + json.dumps(os.path.join(root, "music")) + "\n"
        "libraryName: " + json.dumps(LIBRARY_NAME) + "\n"
        "dataDir: " + json.dumps(os.path.join(root, "data")) + "\n"
        "listenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\n"
        "mdns:\n  enabled: false\n"
        "tailscale:\n  mode: tsnet\n"
    )
    if key:
        body += "  uploadDiagnosticLogs: true\n"
    with open(cfg, "w") as f:
        f.write(body)
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": os.path.join(root, "home"),
        "HTTPS_PROXY": "", "HTTP_PROXY": "",  # set per run
        "TS_DEBUG_ALWAYS_USE_DERP": "1",
        "GODEBUG": "netdns=go",
    }
    if inherited_off:
        env["TS_NO_LOGS_NO_SUPPORT"] = "true"
    return root, cfg, env


def run_bridge(bridge, root, cfg, env, window, label):
    proxy = serve_proxy()
    try:
        env = dict(env)
        env["HTTPS_PROXY"] = env["HTTP_PROXY"] = "http://127.0.0.1:%d" % proxy.port
        log_path = os.path.join(root, label + ".log")
        with open(log_path, "w") as log:
            t0 = time.monotonic()
            p = subprocess.Popen([sys.executable, "-I", os.path.abspath(__file__), "sandbox", str(proxy.port), "--",
                                  os.path.abspath(bridge), "serve", "--config", cfg, "--addr", "127.0.0.1:0"],
                                 stdout=log, stderr=subprocess.STDOUT, env=env, cwd=root)
            early_exit = None
            while time.monotonic() - t0 < window:
                if p.poll() is not None:
                    early_exit = p.returncode
                    break
                time.sleep(0.2)
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(timeout=30)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
        seen = proxy.since(t0)
    finally:
        proxy.shutdown()
        proxy.server_close()
    with open(log_path, errors="replace") as f:
        text = f.read()
    names = re.findall(r'tsnet starting with hostname \\?"([^"\\]*)\\?"', text)
    targets = {}
    for t, method, target in seen:
        key = method + " " + target
        first, count = targets.get(key, (t, 0))
        targets[key] = (min(first, t), count + 1)
    uploads = [t for t, m, tgt in seen if m == "CONNECT" and tgt == LOG_HOST]
    return {
        "label": label,
        "window_s": window,
        "early_exit": early_exit,
        "log_connects": len(uploads),
        "first_log_connect_s": round(min(uploads), 2) if uploads else None,
        "targets": {k: {"first_s": round(v[0], 2), "count": v[1]} for k, v in sorted(targets.items())},
        "node_names": names,
        "log": log_path,
    }


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "sandbox":
        return sandbox_main(sys.argv[2:])
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bridge", required=True, help="the bridge binary, a non-test build")
    ap.add_argument("--work", required=True, help="a directory of its own for the throwaway installs")
    ap.add_argument("--window", type=float, default=60, help="seconds each run is watched")
    ap.add_argument("--rounds", type=int, default=2, help="how many times each arm runs")
    args = ap.parse_args()
    os.makedirs(args.work, exist_ok=True)

    proxy = serve_proxy()
    try:
        sandboxed, probe = check_sandbox(proxy)
        stray = proxy.since(0)
    finally:
        proxy.shutdown()
        proxy.server_close()
    print("sandbox check:", "ok" if sandboxed else "FAILED", json.dumps(probe))
    if not sandboxed or any(m for _, m, _ in stray):
        print("the sandbox does not hold here; no bridge was run")
        return 2

    arms = [("allowed", True, False), ("absent", False, False), ("env-off", True, True)]
    runs = []
    for r in range(args.rounds):
        for name, key, inherited_off in arms:
            label = "%s-%d" % (name, r + 1)
            root, cfg, env = write_install(args.work, label, key, inherited_off)
            res = run_bridge(args.bridge, root, cfg, env, args.window, "first-start")
            res.update(arm=name, round=r + 1)
            runs.append(res)
            if name == "absent" and r == 0:
                # The second start of a node whose first start wrote its state
                # and registered nothing.
                again = run_bridge(args.bridge, root, cfg, env, min(args.window, 20), "second-start")
                again.update(arm="absent, second start", round=r + 1)
                runs.append(again)

    allowed = [x for x in runs if x["arm"] == "allowed"]
    firsts = [x["first_log_connect_s"] for x in allowed if x["first_log_connect_s"] is not None]
    checks = {
        "every allowed run asked for log.tailscale.com": all(x["log_connects"] >= 1 for x in allowed),
        "no run without the key, or with TS_NO_LOGS_NO_SUPPORT=true, asked for it":
            all(x["log_connects"] == 0 for x in runs if x["arm"] != "allowed"),
        "every run asked for Tailscale's coordination server":
            all(any("controlplane.tailscale.com" in k for k in x["targets"]) for x in runs),
        "the window is at least five times the slowest first upload attempt":
            bool(firsts) and args.window >= 5 * max(firsts),
        "every start named the node 1-bit-bridge, never the library":
            all(x["node_names"] and set(x["node_names"]) == {DEFAULT_NAME} for x in runs),
        "no bridge exited before its window ended": all(x["early_exit"] is None for x in runs),
    }
    print()
    print("%-24s %5s %8s %10s  %s" % ("run", "round", "log host", "first (s)", "targets (first s, count)"))
    for x in runs:
        tg = ", ".join("%s (%.1f, %d)" % (k, v["first_s"], v["count"]) for k, v in x["targets"].items())
        print("%-24s %5d %8d %10s  %s" % (x["arm"], x["round"], x["log_connects"],
                                          x["first_log_connect_s"] if x["first_log_connect_s"] is not None else "-", tg))
    print()
    for k, v in checks.items():
        print("%-6s %s" % ("ok" if v else "FAILED", k))
    with open(os.path.join(args.work, "evidence.json"), "w") as f:
        json.dump({"sandbox": probe, "runs": runs, "checks": checks}, f, indent=2)
    return 0 if all(checks.values()) else 1


if __name__ == "__main__":
    sys.exit(main())

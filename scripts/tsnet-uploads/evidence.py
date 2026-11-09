#!/usr/bin/env python3
"""Evidence, from a production binary, that the embedded Tailscale node
uploads its diagnostic logs only where bridge.yaml allows it (backlog B749).

The repository's own test cases cannot see an upload: upstream's logger is
never started inside a test binary. This script runs a bridge binary as an
operator would, in tsnet mode, and counts what it asks a local proxy to
reach:

- Every run sends its HTTP and HTTPS through a proxy on 127.0.0.1 that logs
  every request it receives (a CONNECT's host and port, a plain request's
  host) and refuses every one. It never connects anywhere.
- The bridge runs confined by port, not by address. Landlock (Linux, ABI 4 or
  later) lets it connect a TCP socket only to the proxy's port number, on any
  address: Landlock's network rules name a port and never an address. A
  seccomp filter refuses it every IPv4 and IPv6 socket that is not plain TCP
  (UDP, ICMP, raw, SCTP, MPTCP) and io_uring. So no name lookup, no STUN probe
  and no TCP connection to any other port can be made; a connection to the
  proxy's port number at another address is not prevented, and would not
  reach the proxy's log, on which the verdict rests.
  TS_DEBUG_ALWAYS_USE_DERP=1, an upstream debug knob, keeps the node from
  binding UDP, which the filter refuses, so the node starts as it would on a
  host whose UDP is blocked; GODEBUG=netdns=go keeps every name lookup in Go's
  own resolver.
- Before any run the confinement is checked, from inside it and from outside:
  inside, a TCP connection to another port on 127.0.0.1 and on ::1 (each with
  a listener there) is refused with Landlock's own EACCES, and every non-TCP
  IPv4 and IPv6 socket with the filter's own EPERM; outside, the same
  connections are made and the UDP sockets created, so the refusals are the
  confinement's. The proxy's port at ::1, where nothing listens, is not
  refused with EACCES: the confinement is by port, as stated. Nothing in the
  check leaves the host.

Pass and fail rest on the CONNECTs to log.tailscale.com, Tailscale's log
host (logtail.DefaultHost), alone: the coordination server's CONNECTs are
expected in every run. With tailscale.uploadDiagnosticLogs: true at least one
must come within the window; with the key absent, and with the key true and
TS_NO_LOGS_NO_SUPPORT=true in the bridge's environment, none may come over the
same window, which must be long beside the time the first one took.

Each run's bridge has a throwaway home, config and data dir under --work,
made for it and never one an earlier run left. The node also says the name
it registers with; a new node, and the second start of a node whose first
start registered nothing, must say 1-bit-bridge, never the library's name.

With --previous, an earlier bridge (the release before this change, or a
build of main from before it), an upgraded install is checked too. Its setup
is not an arm: the earlier bridge runs once without the key on a fresh
install, and must run its whole window, ask for log.tailscale.com, and leave
a non-empty log buffer (tailscaled.log*.txt) in the node's state dir. Then
the earlier bridge runs again on a copy of that data dir, and must ask for
the log host again (the positive control on that state), and --bridge runs
on the data dir itself, without the key, and must not ask for it in the
whole window.

usage:
  python3 -I evidence.py --bridge PATH --work DIR [--previous PATH] [--window S] [--rounds N]

Linux only. Exit status 0 when every expectation held. The result is printed
as a table and saved as JSON in the work dir.
"""

import argparse
import ctypes
import errno
import hashlib
import json
import os
import platform
import re
import shutil
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
AF_INET, AF_INET6, SOCK_STREAM, IPPROTO_TCP = 2, 10, 1, 6
LANDLOCK_CREATE_RULESET_VERSION = 1
LANDLOCK_ACCESS_NET_CONNECT_TCP = 1 << 1
LANDLOCK_RULE_NET_PORT = 2

ARCH = {
    # machine: audit arch, socket, io_uring_setup, landlock create/add/restrict, ELF e_machine
    "x86_64": (0xC000003E, 41, 425, 444, 445, 446, 0x3E),
    "aarch64": (0xC00000B7, 198, 425, 444, 445, 446, 0xB7),
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
    """A filter that refuses, with EPERM, every IPv4 and IPv6 socket that is
    not plain TCP (protocol 0 or IPPROTO_TCP: UDP, ICMP, raw, SCTP and MPTCP
    are refused, and Go falls back to plain TCP from MPTCP), refuses io_uring,
    and kills a process calling in through another ABI."""
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
        ("deny", RET, None, None, SECCOMP_RET_ERRNO | errno.EPERM),
    ])


def apply_sandbox(port):
    """Confine this process, and what it executes, by port and not by
    address: Landlock lets it connect a TCP socket only to port, at any
    address (its network rules name a port and never an address), and a
    seccomp filter refuses it every IPv4 and IPv6 socket that is not plain
    TCP, and io_uring. Raises where the kernel cannot."""
    machine = platform.machine()
    if sys.platform != "linux" or machine not in ARCH:
        raise SystemExit("the sandbox needs Linux on x86_64 or aarch64")
    arch, nr_socket, nr_io_uring, nr_create, nr_add, nr_restrict, _ = ARCH[machine]
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


def sandboxed(port, argv):
    """The command line that runs argv inside the sandbox for port."""
    return [sys.executable, "-I", os.path.abspath(__file__), "sandbox", str(port), "--"] + argv


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


# --- the confinement's own check, inside it and outside, touching nothing off the host ---

PROBE = r"""
import errno, json, socket, sys
port, other4, other6 = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
out = {}
def word(e):
    return "refused: " + errno.errorcode.get(e.errno, str(e.errno))
def tcp(family, host, p):
    s = socket.socket(family, socket.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect((host, p)); return "connected"
    except OSError as e:
        return word(e)
    finally:
        s.close()
out["tcp4 to the proxy's port on 127.0.0.1"] = tcp(socket.AF_INET, "127.0.0.1", port)
out["tcp4 to another port on 127.0.0.1"] = tcp(socket.AF_INET, "127.0.0.1", other4)
if other6:
    out["tcp6 to another port on ::1"] = tcp(socket.AF_INET6, "::1", other6)
    out["tcp6 to the proxy's port on ::1"] = tcp(socket.AF_INET6, "::1", port)
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
        out[name + " socket"] = word(e)
print(json.dumps(out))
"""

LANDLOCK_DENIAL = "refused: EACCES"
FILTER_DENIAL = "refused: EPERM"


def listener(family, host):
    """A listening socket on host at a port of the kernel's choosing, or None
    where the host has no such address (no IPv6 loopback)."""
    try:
        s = socket.socket(family, socket.SOCK_STREAM)
        s.bind((host, 0))
        s.listen(8)
        return s
    except OSError:
        return None


def check_sandbox(proxy):
    """The probe run inside the sandbox and outside it, and the verdict: a
    list of (what, held) and the two probes' answers."""
    l4, l6 = listener(socket.AF_INET, "127.0.0.1"), listener(socket.AF_INET6, "::1")
    if l4 is None:
        raise SystemExit("no listener can be bound on 127.0.0.1")
    other4 = l4.getsockname()[1]
    other6 = l6.getsockname()[1] if l6 else 0
    probe = [sys.executable, "-I", "-c", PROBE, str(proxy.port), str(other4), str(other6)]
    answers = {}
    try:
        for where, cmd in (("inside", sandboxed(proxy.port, probe)), ("outside", probe)):
            out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
            if out.returncode != 0:
                raise SystemExit("the sandbox check did not run " + where + ": " + out.stderr)
            answers[where] = json.loads(out.stdout)
    finally:
        l4.close()
        if l6:
            l6.close()
    inside, outside = answers["inside"], answers["outside"]
    other_ports = ["tcp4 to another port on 127.0.0.1"] + (["tcp6 to another port on ::1"] if other6 else [])
    non_tcp = ["udp4 socket", "udp6 socket", "icmp4 socket", "raw4 socket", "sctp4 socket", "mptcp4 socket"]
    verdict = [
        ("inside: a TCP connection to the proxy's port on 127.0.0.1 is made",
         inside.get("tcp4 to the proxy's port on 127.0.0.1") == "connected"),
        ("inside: a TCP connection to another port is refused with Landlock's EACCES (" +
         ", ".join(other_ports) + ")",
         all(inside.get(k) == LANDLOCK_DENIAL for k in other_ports)),
        ("inside: every IPv4 and IPv6 socket that is not plain TCP is refused with the filter's EPERM",
         all(inside.get(k) == FILTER_DENIAL for k in non_tcp)),
        ("inside: a TCP socket and a Unix socket are still created",
         inside.get("tcp6 socket") == "created" and inside.get("unix socket") == "created"),
        ("outside: the same connections are made and the UDP sockets created, so the refusals are the confinement's",
         all(outside.get(k) == "connected" for k in other_ports) and
         outside.get("udp4 socket") == "created" and outside.get("udp6 socket") == "created"),
    ]
    if other6:
        verdict.append(("inside: the proxy's port at ::1 is not refused by Landlock: the confinement is by port, "
                        "not by address", inside.get("tcp6 to the proxy's port on ::1") != LANDLOCK_DENIAL))
    return verdict, {"inside": inside, "outside": outside, "ipv6_loopback": bool(other6)}


# --- the binaries ---

def elf_for_this_machine(path):
    """Whether path is a 64-bit little-endian ELF for this machine."""
    with open(path, "rb") as f:
        head = f.read(20)
    want = ARCH.get(platform.machine(), (None,) * 7)[6]
    return (len(head) == 20 and head[:4] == b"\x7fELF" and head[4] == 2 and head[5] == 1 and
            int.from_bytes(head[18:20], "little") == want)


def describe(path):
    """What a binary is: its digest, whether it is for this machine, and what
    its version command says (run confined, to a port nothing listens on)."""
    with open(path, "rb") as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    out = subprocess.run(sandboxed(9, [os.path.abspath(path), "version"]), capture_output=True, text=True,
                         timeout=60, env={"PATH": "/usr/bin:/bin"})
    return {"path": os.path.abspath(path), "sha256": digest, "elf_for_this_machine": elf_for_this_machine(path),
            "version": (out.stdout.strip().splitlines() or [""])[0]}


# --- the runs ---

def write_install(work, name, key, inherited_off):
    """A throwaway install of its own under work: made here, never one an
    earlier run left (a work dir that already holds name is refused)."""
    root = os.path.join(work, name)
    if os.path.lexists(root):
        raise SystemExit(f"{root} is there already: give --work a directory of its own")
    for d in ("home", "music", "data"):
        os.makedirs(os.path.join(root, d))
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


def log_buffer(root):
    """The node's log buffer as the state dir holds it: each
    tailscaled.log*.txt and its size in bytes."""
    state = os.path.join(root, "data", "tailscale")
    names = sorted(os.listdir(state)) if os.path.isdir(state) else []
    return {n: os.path.getsize(os.path.join(state, n)) for n in names
            if n.startswith("tailscaled.log") and n.endswith(".txt")}


def run_bridge(bridge, root, cfg, env, window, label):
    proxy = serve_proxy()
    try:
        env = dict(env)
        env["HTTPS_PROXY"] = env["HTTP_PROXY"] = "http://127.0.0.1:%d" % proxy.port
        log_path = os.path.join(root, label + ".log")
        with open(log_path, "w") as log:
            t0 = time.monotonic()
            p = subprocess.Popen(sandboxed(proxy.port, [os.path.abspath(bridge), "serve", "--config", cfg,
                                                        "--addr", "127.0.0.1:0"]),
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
        "binary": os.path.abspath(bridge),
        "window_s": window,
        "early_exit": early_exit,
        "log_connects": len(uploads),
        "first_log_connect_s": round(min(uploads), 2) if uploads else None,
        "targets": {k: {"first_s": round(v[0], 2), "count": v[1]} for k, v in sorted(targets.items())},
        "node_names": names,
        "log_buffer_after": log_buffer(root),
        "log": log_path,
    }


def upgrade(args, runs):
    """The upgraded install: the earlier bridge's setup on a fresh install,
    its positive control on a copy of that data dir, and this bridge on the
    data dir itself. Returns the checks that are the upgrade's own."""
    root, cfg, env = write_install(args.work, "upgrade", key=False, inherited_off=False)
    setup = run_bridge(args.previous, root, cfg, env, args.window, "previous-first-start")
    setup.update(arm="upgrade setup: previous", round=1)
    runs.append(setup)
    left = log_buffer(root)

    copy_root, copy_cfg, copy_env = write_install(args.work, "upgrade-copy", key=False, inherited_off=False)
    shutil.rmtree(os.path.join(copy_root, "data"))
    shutil.copytree(os.path.join(root, "data"), os.path.join(copy_root, "data"), symlinks=True)
    again = run_bridge(args.previous, copy_root, copy_cfg, copy_env, args.window, "previous-again-on-a-copy")
    again.update(arm="upgrade control: previous again", round=1)
    runs.append(again)

    upgraded = run_bridge(args.bridge, root, cfg, env, args.window, "this-build-on-the-upgraded-data-dir")
    upgraded.update(arm="upgrade: this build", round=1, log_buffer_before=left)
    runs.append(upgraded)
    return [
        ("upgrade setup: the previous build ran its whole window", setup["early_exit"] is None),
        ("upgrade setup: the previous build asked for log.tailscale.com", setup["log_connects"] >= 1),
        ("upgrade setup: the previous build left a non-empty log buffer in the node's state dir",
         bool(left) and sum(left.values()) > 0),
        ("upgrade control: the previous build, again on a copy of that data dir, asked for log.tailscale.com",
         again["early_exit"] is None and again["log_connects"] >= 1),
        ("upgrade: this build, without the key on the data dir the previous build left, never asked for "
         "log.tailscale.com in its whole window", upgraded["early_exit"] is None and upgraded["log_connects"] == 0),
    ]


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "sandbox":
        return sandbox_main(sys.argv[2:])
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bridge", required=True, help="the bridge binary, a non-test build")
    ap.add_argument("--previous", help="an earlier bridge, from before this change, for the upgraded install")
    ap.add_argument("--work", required=True, help="a directory of its own for the throwaway installs")
    ap.add_argument("--window", type=float, default=60, help="seconds each run is watched")
    ap.add_argument("--rounds", type=int, default=2, help="how many times each arm runs")
    args = ap.parse_args()
    # Each bridge runs in its install's folder, so every path it is given is
    # absolute.
    args.work = os.path.abspath(args.work)
    os.makedirs(args.work, exist_ok=True)

    binaries = {"bridge": describe(args.bridge)}
    if args.previous:
        binaries["previous"] = describe(args.previous)
    print("binaries:", json.dumps(binaries))

    proxy = serve_proxy()
    try:
        verdict, probe = check_sandbox(proxy)
        stray = proxy.since(0)
    finally:
        proxy.shutdown()
        proxy.server_close()
    for what, held in verdict:
        print("%-6s %s" % ("ok" if held else "FAILED", what))
    if not all(held for _, held in verdict) or any(m for _, m, _ in stray):
        print("the sandbox does not hold here; no bridge was run", json.dumps(probe))
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
                # and registered nothing, watched as long as every other run.
                again = run_bridge(args.bridge, root, cfg, env, args.window, "second-start")
                again.update(arm="absent, second start", round=r + 1)
                runs.append(again)
    upgrade_checks = upgrade(args, runs) if args.previous else []

    # Every check of this build's runs is over this build's runs alone: the
    # previous build's are the upgrade's setup and control, with their own.
    ours = [x for x in runs if x["binary"] == os.path.abspath(args.bridge)]
    allowed = [x for x in ours if x["arm"] == "allowed"]
    firsts = [x["first_log_connect_s"] for x in allowed if x["first_log_connect_s"] is not None]
    firsts += [x["first_log_connect_s"] for x in runs
               if x["arm"] == "upgrade control: previous again" and x["first_log_connect_s"] is not None]
    checks = dict(verdict)
    checks.update({
        "every allowed run asked for log.tailscale.com": all(x["log_connects"] >= 1 for x in allowed),
        "no run of this build without the key, or with TS_NO_LOGS_NO_SUPPORT=true, asked for it":
            all(x["log_connects"] == 0 for x in ours if x["arm"] != "allowed"),
        "every run of this build asked for Tailscale's coordination server":
            all(any("controlplane.tailscale.com" in k for k in x["targets"]) for x in ours),
        "the window is at least five times the slowest first upload attempt of the positive controls":
            bool(firsts) and args.window >= 5 * max(firsts),
        "every start of this build named the node 1-bit-bridge, never the library":
            all(x["node_names"] and set(x["node_names"]) == {DEFAULT_NAME} for x in ours),
        "no run of this build exited before its window ended": all(x["early_exit"] is None for x in ours),
    })
    checks.update(dict(upgrade_checks))
    print()
    print("%-34s %5s %8s %10s  %s" % ("run", "round", "log host", "first (s)", "targets (first s, count)"))
    for x in runs:
        tg = ", ".join("%s (%.1f, %d)" % (k, v["first_s"], v["count"]) for k, v in x["targets"].items())
        print("%-34s %5d %8d %10s  %s" % (x["arm"], x["round"], x["log_connects"],
                                          x["first_log_connect_s"] if x["first_log_connect_s"] is not None else "-", tg))
    print()
    for k, v in checks.items():
        print("%-6s %s" % ("ok" if v else "FAILED", k))
    with open(os.path.join(args.work, "evidence.json"), "w") as f:
        json.dump({"binaries": binaries, "sandbox": probe, "runs": runs, "checks": checks}, f, indent=2)
    return 0 if all(checks.values()) else 1


if __name__ == "__main__":
    sys.exit(main())

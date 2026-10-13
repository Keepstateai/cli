#!/usr/bin/env python3
"""Drive a program in a pseudo-terminal like a person: a script of
(expect-text, send-bytes, options...) steps. Prints the whole screen
transcript; exits 0 when every expectation was met, 1 with the step that was
not.

Options after the two fields:
  new          look for the text only in what arrived since the last send
  any          the text is several alternatives separated by "||"
  soft         a missed expectation is noted and the script carries on
  wait:SECS    let the screen settle this long before sending
  snap:NAME    write the raw bytes so far to OUTDIR/NAME.raw (argv[2])

The spec may set "rows" and "cols" (default 30 x 100)."""
import os, pty, select, sys, time, json


def main():
    spec = json.load(open(sys.argv[1]))
    outdir = sys.argv[2] if len(sys.argv) > 2 else ""
    rows, cols = spec.get("rows", 30), spec.get("cols", 100)
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update(spec["env"])
        os.execv(spec["argv"][0], spec["argv"])
    import fcntl, termios, struct
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    buf = b""
    sent_at = 0

    def pump(t):
        nonlocal buf
        end = time.time() + t
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if r:
                try:
                    d = os.read(fd, 65536)
                except OSError:
                    return False
                if not d:
                    return False
                buf += d
        return True

    def seen(want, start, opts):
        wants = want.split("||") if "any" in opts else [want]
        return any(w.encode() in buf[start:] for w in wants)

    ok = True
    for i, step in enumerate(spec["steps"]):
        want, send, opts = step[0], step[1], step[2:]
        deadline = time.time() + spec.get("timeout", 10)
        start = sent_at if "new" in opts else 0
        while want and not seen(want, start, opts) and time.time() < deadline:
            if not pump(0.1):
                break
        if want and not seen(want, start, opts):
            if "soft" in opts:
                print("\n@@ STEP %d: never saw %r (soft: carrying on)" % (i, want), file=sys.stderr)
            else:
                sys.stdout.write(buf.decode("utf-8", "replace"))
                print("\n@@ STEP %d: never saw %r" % (i, want))
                ok = False
                break
        for opt in opts:
            if opt.startswith("wait:"):
                pump(float(opt[5:]))
        for opt in opts:
            if opt.startswith("snap:") and outdir:
                pump(0.4)
                open(os.path.join(outdir, opt[5:] + ".raw"), "wb").write(buf)
        if send:
            sent_at = len(buf)
            os.write(fd, send.encode("utf-8"))
            pump(0.2)
    pump(0.5)
    if outdir:
        open(os.path.join(outdir, "final.raw"), "wb").write(buf)
    sys.stdout.write(buf.decode("utf-8", "replace"))
    sys.exit(0 if ok else 1)


main()

#!/usr/bin/env python3
"""Drive a program in a pseudo-terminal like a person: a script of
(expect-text, send-bytes) steps. Prints the whole screen transcript; exits 0
when every expectation was met, 1 with the step that was not."""
import os, pty, select, sys, time, json

def main():
    spec = json.load(open(sys.argv[1]))
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update(spec["env"])
        os.execv(spec["argv"][0], spec["argv"])
    import fcntl, termios, struct
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))
    buf = b""
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
    ok = True
    for i, step in enumerate(spec["steps"]):
        want, send = step[0], step[1]
        deadline = time.time() + spec.get("timeout", 10)
        start = len(buf) if step[2:] and step[2] == "new" else 0
        while want and want.encode() not in buf[start:] and time.time() < deadline:
            if not pump(0.1):
                break
        if want and want.encode() not in buf[start:]:
            sys.stdout.write(buf.decode("utf-8", "replace"))
            print("\n@@ STEP %d: never saw %r" % (i, want))
            ok = False
            break
        if send:
            os.write(fd, send.encode("utf-8"))
            pump(0.2)
    pump(0.5)
    sys.stdout.write(buf.decode("utf-8", "replace"))
    sys.exit(0 if ok else 1)

main()

"""Acquire a crash-released advisory lock, then replace this process with Node."""

import fcntl
import os
import sys
import time


def main():
    if len(sys.argv) < 4:
        raise SystemExit("usage: lock_exec.py LOCK_PATH COMMAND [ARGS...]")
    fd = os.open(sys.argv[1], os.O_RDWR | os.O_CREAT, 0o600)
    deadline = time.monotonic() + 60
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            break
        except BlockingIOError:
            if time.monotonic() >= deadline:
                raise SystemExit("publication lock busy")
            time.sleep(0.1)
    os.set_inheritable(fd, True)
    env = os.environ.copy()
    env["REGISTRY_SEARCH_LOCK_FD"] = str(fd)
    env["REGISTRY_SEARCH_LOCK_PID"] = str(os.getpid())
    os.execvpe(sys.argv[2], sys.argv[2:], env)


if __name__ == "__main__":
    main()

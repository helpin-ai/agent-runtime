#!/usr/bin/env python3
"""Exercise full-screen startup, commands, resize and restoration in a real PTY."""
import fcntl
import os
import pty
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time

binary = os.path.abspath(sys.argv[1])
with tempfile.TemporaryDirectory(prefix="cli-terminal-") as home:
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    env = dict(os.environ, TERM="xterm-256color", AGENT_RUNTIME_CLI_HOME=home,
               OPENROUTER_API_KEY="fixture-only", OPENAI_API_KEY="", ANTHROPIC_API_KEY="",
               AGENT_RUNTIME_NATIVE_PROVIDER="", AGENT_RUNTIME_NATIVE_MODEL="")
    process = subprocess.Popen([binary], stdin=slave, stdout=slave, stderr=slave, env=env)
    os.close(slave)
    transcript = bytearray()
    def drain(seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                transcript.extend(chunk)
                # Answer terminal capability probes needed by Bubble Tea startup.
                if b"\x1b[6n" in chunk:
                    os.write(master, b"\x1b[1;1R")
                if b"\x1b[c" in chunk:
                    os.write(master, b"\x1b[?1;2c")
    try:
        drain(2)
        for command in [b"/help\r", b"/models\r", b"/runs\r"]:
            os.write(master, command)
            drain(0.4)
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 12, 42, 0, 0))
        process.send_signal(signal.SIGWINCH)
        drain(0.4)
        os.write(master, b"\x1b[5~")
        drain(0.2)
        os.write(master, b"/quit\r")
        drain(1)
        process.wait(timeout=5)
        raw = bytes(transcript)
        assert b"\x1b[?1049h" in raw, "alternate screen not entered"
        assert b"\x1b[?1049l" in raw, "terminal screen not restored"
        assert b"openrouter" in raw, "provider was not detected"
        assert b"/resume" in raw, "help command not rendered"
        assert process.returncode == 0, raw[-3000:]
        print("PASS: full-screen startup, automatic OpenRouter selection, help/history, resize, scrolling, terminal restoration")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)

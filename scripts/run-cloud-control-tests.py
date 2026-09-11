#!/usr/bin/env python3
"""Run cloud-control's PostgreSQL-backed tests against the deployment database.

`internal/cloud/mobile_stream_runtime_test.go` skips itself unless
MILEVIA_TEST_DATABASE_URL is set, because the event-stream logic it covers
(LISTEN/NOTIFY delivery and the drain query) cannot be exercised without a real
PostgreSQL. The deployment's database only listens on the server's loopback, so
this opens an SSH tunnel and passes the DSN in.

The tests themselves only ever write rows belonging to a throwaway instance id
(`test-stream-*`) and delete them again, so pointing this at production is safe.
It needs no writes of its own: the deployment file is read to obtain the DSN and
the password is never printed.

Usage (PowerShell):
    $env:SSH_HOST="111.229.52.158"; $env:SSH_USER="root"
    $env:SSH_PASS="<server password>"
    python scripts/run-cloud-control-tests.py

Requires: paramiko (`pip install paramiko`) and a Go toolchain on PATH.
"""
import os
import re
import socket
import socketserver
import subprocess
import sys
import threading

import paramiko

LOCAL_PORT = 15432
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CLOUD_DIR = os.path.join(REPO_ROOT, "apps", "cloud-control")
REMOTE_ENV_FILE = "/root/projects/milevia/infrastructure/.env.server"
TEST_PATTERN = "MobileStream|DrainInstanceEvents"


class ForwardServer(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True


class ForwardHandler(socketserver.BaseRequestHandler):
    """Pumps one accepted local connection through an SSH direct-tcpip channel."""

    def handle(self):
        try:
            channel = self.server.transport.open_channel(
                "direct-tcpip",
                (self.server.remote_host, self.server.remote_port),
                self.request.getpeername(),
            )
        except Exception as exc:  # pragma: no cover - transport level failure
            print("tunnel open failed:", exc)
            return
        if channel is None:
            print("tunnel refused by the server")
            return

        def from_socket():
            try:
                while True:
                    data = self.request.recv(65536)
                    if not data:
                        break
                    channel.sendall(data)
            except Exception:
                pass
            finally:
                try:
                    channel.shutdown_write()
                except Exception:
                    pass

        thread = threading.Thread(target=from_socket, daemon=True)
        thread.start()
        try:
            while True:
                data = channel.recv(65536)
                if not data:
                    break
                self.request.sendall(data)
        except Exception:
            pass
        finally:
            try:
                self.request.shutdown(socket.SHUT_WR)
            except Exception:
                pass
        thread.join(timeout=1)


def read_database_url(client):
    _, stdout, _ = client.exec_command(
        f"grep -o 'MILEVIA_CLOUD_DATABASE_URL=.*' {REMOTE_ENV_FILE}", timeout=30
    )
    raw = stdout.read().decode("utf-8", "replace").strip()
    if "=" not in raw:
        raise SystemExit("could not read MILEVIA_CLOUD_DATABASE_URL from the deployment file")
    return raw.split("=", 1)[1]


def main():
    host = os.environ.get("SSH_HOST")
    user = os.environ.get("SSH_USER")
    password = os.environ.get("SSH_PASS")
    if not (host and user and password):
        raise SystemExit("set SSH_HOST, SSH_USER and SSH_PASS")

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(host, username=user, password=password, timeout=20)

    url = read_database_url(client)
    credentials = re.search(r"//([^:]+):([^@]+)@", url)
    endpoint = re.search(r"@([^:/]+):(\d+)/(\w+)", url)
    if not credentials or not endpoint:
        raise SystemExit("could not parse the database URL")
    db_user, db_pass, db_name = credentials.group(1), credentials.group(2), endpoint.group(3)
    print(f"database: db={db_name} user={db_user} password=<redacted>")

    transport = client.get_transport()
    server = ForwardServer(("127.0.0.1", LOCAL_PORT), ForwardHandler)
    server.transport = transport
    server.remote_host = "127.0.0.1"
    server.remote_port = 5432
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print(f"tunnel: 127.0.0.1:{LOCAL_PORT} -> 127.0.0.1:5432 on {host}")

    env = dict(os.environ)
    env["MILEVIA_TEST_DATABASE_URL"] = (
        f"postgres://{db_user}:{db_pass}@127.0.0.1:{LOCAL_PORT}/{db_name}"
    )
    env.setdefault("GOPROXY", "https://goproxy.cn,direct")
    env["GOFLAGS"] = "-mod=mod"

    try:
        completed = subprocess.run(
            ["go", "test", "./internal/cloud/", "-run", TEST_PATTERN,
             "-count=1", "-v", "-timeout", "180s"],
            cwd=CLOUD_DIR, env=env, capture_output=True, text=True,
            encoding="utf-8", errors="replace", timeout=900,
        )
        print(completed.stdout)
        if completed.stderr.strip():
            print("stderr:", completed.stderr[:4000])
        return completed.returncode
    finally:
        server.shutdown()
        transport.close()
        client.close()


if __name__ == "__main__":
    sys.exit(main())

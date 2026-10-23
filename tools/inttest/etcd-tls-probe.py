"""Ask an etcd port whether it accepts a client certificate.

Usage: python3 etcd-tls-probe.py IP CERT KEY [PORT]

PORT is the client port, 2379, unless given; the peer port is 2380.

Prints the HTTP status line of GET /version on PORT when etcd accepted the
certificate, "rejected: ..." when etcd answered with a TLS alert refusing it,
and "error: ..." when three attempts reached no decision. The server
certificate is not verified.

Only the alerts a TLS server sends for a client certificate it will not
accept count as a rejection, the same three internal/pki/etcdprobe.go's
isCertRejection accepts. Any other alert (protocol version, internal error)
can arrive before the certificate is checked, so it is retried as an error.
"""

import socket
import ssl
import sys
import time

ip, cert, key = sys.argv[1:4]
port = int(sys.argv[4]) if len(sys.argv) > 4 else 2379
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
ctx.load_cert_chain(cert, key)
host = f"[{ip}]" if ":" in ip else ip
REJECTIONS = ("ALERT_BAD_CERTIFICATE", "ALERT_UNKNOWN_CA", "ALERT_CERTIFICATE_REQUIRED")


def attempt():
    try:
        with ctx.wrap_socket(socket.create_connection((ip, port), timeout=10)) as s:
            s.sendall(f"GET /version HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n".encode())
            data = s.recv(200)
    except ssl.SSLError as e:
        if any(a in (e.reason or "").upper() for a in REJECTIONS):
            return f"rejected: {e.reason}"
        return f"error: {e}"
    except OSError as e:
        return f"error: {e}"
    if data.startswith(b"HTTP/"):
        return data.split(b"\r\n")[0].decode()
    return "error: no HTTP response"


for n in range(3):
    result = attempt()
    if not result.startswith("error:"):
        break
    time.sleep(2)
print(result)

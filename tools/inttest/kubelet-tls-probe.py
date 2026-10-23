"""Ask a kubelet's :10250 whether it accepts a client certificate.

Usage: python3 kubelet-tls-probe.py IP CERT KEY

Prints the HTTP status line of GET /healthz: 200 when the kubelet
authenticated the certificate, 401 when it did not. A TLS or connection
failure prints "error: ..." and proves nothing either way. The server
certificate is not verified.
"""

import socket
import ssl
import sys

ip, cert, key = sys.argv[1:4]
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
ctx.load_cert_chain(cert, key)
host = f"[{ip}]" if ":" in ip else ip
try:
    with ctx.wrap_socket(socket.create_connection((ip, 10250), timeout=10)) as s:
        s.sendall(f"GET /healthz HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n".encode())
        data = s.recv(200)
    print(data.split(b"\r\n")[0].decode() if data else "error: connection closed")
except (ssl.SSLError, ConnectionError, OSError) as e:
    print(f"error: {e}")

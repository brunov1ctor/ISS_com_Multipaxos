"""Mesmo cenário do examples/c/main.c, usando a mesma biblioteca via ctypes.

A ordem total (local/global) é do sistema, escolhida no deploy (./deploy.sh ... --order global).

Uso: python3 exemplo.py <libiss_multicast.so> <config.yml> <discovery host:port> [N]
     N = quantidade de multicasts em paralelo, alternando G1/G2 (carga)
"""
import ctypes
import sys
from concurrent.futures import ThreadPoolExecutor

lib = ctypes.CDLL(sys.argv[1])
lib.iss_get.argtypes = [ctypes.c_uint, ctypes.c_char_p, ctypes.POINTER(ctypes.c_void_p)]
lib.iss_last_error.restype = ctypes.c_void_p
lib.iss_free.argtypes = [ctypes.c_void_p]


def check(rc, what):
    if rc < 0:
        p = lib.iss_last_error()
        print(f"{what} falhou: {ctypes.string_at(p).decode()}", file=sys.stderr)
        lib.iss_free(p)


def get(group, key):
    out = ctypes.c_void_p()
    rc = lib.iss_get(group, key.encode(), ctypes.byref(out))
    check(rc, f"GET({key})")
    if rc == 1:
        v = ctypes.string_at(out).decode()
        lib.iss_free(out)
        return v
    return None


if lib.iss_connect(sys.argv[2].encode(), sys.argv[3].encode()) < 0:
    check(-1, "iss_connect")
    sys.exit(1)

check(lib.iss_put(1, b"x", b"10"), "PUT(x,10)")
check(lib.iss_put(2, b"y", b"10"), "PUT(y,10)")
print("GET(x) =", get(1, "x"))
print("GET(y) =", get(2, "y"))

if len(sys.argv) > 4:
    n = int(sys.argv[4])

    def send(i):
        g = 1 + i % 2
        msg = f"MSG-{i}-G{g}"
        check(lib.iss_multicast(g, msg.encode()), msg)

    with ThreadPoolExecutor(max_workers=8) as ex:
        list(ex.map(send, range(n)))
    print(f"{n} multicasts enviados (G1/G2 alternados)")

lib.iss_close()

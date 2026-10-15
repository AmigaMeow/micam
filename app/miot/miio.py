#!/usr/bin/env python3
"""miIO 通用客户端（修正版）。"""
import binascii, hashlib, json, socket, struct, time
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

class MiIO:
    def __init__(self, ip, token_hex, timeout=8):
        self.ip = ip
        self.token = binascii.unhexlify(token_hex)
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.settimeout(timeout)
        self.did = None
        self.stamp = None
        self._handshake()

    @staticmethod
    def _md5(b): return hashlib.md5(b).digest()

    def _key_iv(self):
        key = self._md5(self.token)
        iv = self._md5(key + self.token)
        return key, iv

    def _handshake(self):
        self.sock.sendto(bytes([0x21, 0x31, 0x00, 0x20]) + b"\xff" * 28, (self.ip, 54321))
        r, _ = self.sock.recvfrom(1024)
        self.did, self.stamp = r[8:12], r[12:16]

    def _enc(self, data):
        key, iv = self._key_iv()
        pad = 16 - (len(data) % 16)
        data = data + bytes([pad] * pad)
        return Cipher(algorithms.AES(key), modes.CBC(iv)).encryptor().update(data)

    def _dec(self, data):
        key, iv = self._key_iv()
        d = Cipher(algorithms.AES(key), modes.CBC(iv)).decryptor()
        out = d.update(data) + d.finalize()
        return out[:-out[-1]] if out else out

    def call(self, method, params=None, retries=2):
        payload = json.dumps({"id": 1, "method": method, "params": params or []},
                             separators=(",", ":")).encode()
        enc = self._enc(payload)
        total = 32 + len(enc)
        header = bytes([0x21, 0x31]) + struct.pack(">H", total) + b"\x00" * 4 + self.did + self.stamp
        checksum = self._md5(header + self.token + enc)
        pkt = header + checksum + enc
        for i in range(retries + 1):
            self.sock.sendto(pkt, (self.ip, 54321))
            try:
                r, _ = self.sock.recvfrom(4096)
                if len(r) > 32:
                    return json.loads(self._dec(r[32:]))
                return {"_empty": True}
            except socket.timeout:
                if i == retries:
                    return {"_timeout": True}
                time.sleep(0.5)

    def close(self):
        self.sock.close()

def batch(ip, tok, spec_file):
    """批量执行多条命令，一次进程搞定。

    spec_file 是 JSON：{"calls": [[method, params], ...]}
    输出 {"results": [r1, r2, ...]}

    为什么需要：每次调用起一个 Python 进程要 ~200ms，
    读 9 个属性就是 1.8 秒。批量后只起一次。
    """
    spec = json.load(open(spec_file))
    m = MiIO(ip, tok)
    out = []
    try:
        for method, params in spec.get("calls", []):
            out.append(m.call(method, params))
    finally:
        m.close()
    return {"results": out}


def main():
    """命令行：miio.py <ip> <token> <method> [params_json]

    批量模式：miio.py <ip> <token> --batch <spec_file>
    """
    import sys
    if len(sys.argv) < 4:
        print(json.dumps({"error": "usage: miio.py <ip> <token> <method> [params_json]"}))
        return 2
    ip, tok, method = sys.argv[1], sys.argv[2], sys.argv[3]
    # 批量模式
    if method == "--batch" and len(sys.argv) > 4:
        try:
            print(json.dumps(batch(ip, tok, sys.argv[4]), ensure_ascii=False))
            return 0
        except Exception as e:
            print(json.dumps({"error": str(e)[:120]}))
            return 4
    params = None
    if len(sys.argv) > 4:
        try:
            params = json.loads(sys.argv[4])
        except Exception:
            params = None
    try:
        m = MiIO(ip, tok)
    except Exception as e:
        print(json.dumps({"error": "handshake: %s" % str(e)[:80]}))
        return 3
    try:
        r = m.call(method, params)
        print(json.dumps(r, ensure_ascii=False))
        return 0
    except Exception as e:
        print(json.dumps({"error": str(e)[:120]}))
        return 4
    finally:
        m.close()


if __name__ == "__main__":
    import sys
    sys.exit(main())

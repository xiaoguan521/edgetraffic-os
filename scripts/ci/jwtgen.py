#!/usr/bin/env python3
"""CI 用 JWT 生成器（P10 验收）：只用标准库与 openssl 命令行。

用法：
  jwtgen.py hs <密钥> [选项]          HS256
  jwtgen.py rs <RSA 私钥 PEM> [选项]  RS256（openssl dgst -sha256 -sign）
  jwtgen.py none                      alg=none（必须被拒绝）
选项：--sub S --iss S --aud S --exp-in 秒（负数表示已过期） --alg 覆盖头部 alg --tamper（篡改签名）
"""
import argparse, base64, hashlib, hmac, json, subprocess, sys, time


def b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("kind", choices=["hs", "rs", "none"])
    p.add_argument("key", nargs="?")
    p.add_argument("--sub", default="alice")
    p.add_argument("--iss")
    p.add_argument("--aud")
    p.add_argument("--exp-in", type=int, default=3600)
    p.add_argument("--alg")
    p.add_argument("--tamper", action="store_true")
    a = p.parse_args()

    alg = a.alg or {"hs": "HS256", "rs": "RS256", "none": "none"}[a.kind]
    claims = {"sub": a.sub, "exp": int(time.time()) + a.exp_in, "iat": int(time.time())}
    if a.iss:
        claims["iss"] = a.iss
    if a.aud:
        claims["aud"] = a.aud
    signing = b64(json.dumps({"alg": alg, "typ": "JWT"}).encode()) + "." + b64(json.dumps(claims).encode())
    if a.kind == "none":
        sig = b""
    elif a.kind == "hs":
        key = a.key.encode()
        if a.key.startswith("@"):  # @文件：用文件内容作密钥（算法混淆测试：拿 RSA 公钥当 HMAC 密钥）
            key = open(a.key[1:], "rb").read()
        sig = hmac.new(key, signing.encode(), hashlib.sha256).digest()
    else:
        sig = subprocess.run(["openssl", "dgst", "-sha256", "-sign", a.key], input=signing.encode(),
                             capture_output=True, check=True).stdout
    if a.tamper and sig:
        sig = bytes([sig[0] ^ 1]) + sig[1:]
    sys.stdout.write(signing + "." + b64(sig))


if __name__ == "__main__":
    main()

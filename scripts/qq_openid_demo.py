#!/usr/bin/env python3
"""最小 QQ 机器人 Demo（仅标准库）：监听 C2C_MESSAGE_CREATE，打印用户 OpenID。

用法（仓库根目录）：
  export QQ_APP_ID=你的AppID
  export QQ_APP_SECRET=你的AppSecret
  python3 scripts/qq_openid_demo.py

可选：
  export QQ_SANDBOX=1   # 使用沙箱网关（默认 1）

首次捕获的 OpenID 会追加写入当前工作目录下的 qq_user_openid.txt
（该文件已加入 .gitignore，勿提交）。
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import os.path
import random
import socket
import ssl
import struct
import sys
import threading
import time
import urllib.error
import urllib.request
from urllib.parse import urlparse


APP_ID = os.environ.get("QQ_APP_ID", "").strip()
APP_SECRET = os.environ.get("QQ_APP_SECRET", "").strip()
USE_SANDBOX = os.environ.get("QQ_SANDBOX", "1").strip() not in ("0", "false", "False")
INTENT_GROUP_AND_C2C = 1 << 25


def http_json(method: str, url: str, body: dict | None = None, headers: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        err = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {e.code} {url}: {err}") from e


def get_access_token(app_id: str, app_secret: str) -> str:
    url = "https://bots.qq.com/app/getAppAccessToken"
    result = http_json("POST", url, {"appId": app_id, "clientSecret": app_secret})
    token = result.get("access_token")
    if not token:
        raise RuntimeError(f"获取 access_token 失败: {result}")
    print(f"[ok] access_token 已获取，约 {result.get('expires_in')} 秒后过期")
    return token


def get_gateway(token: str, sandbox: bool) -> str:
    host = "sandbox.api.sgroup.qq.com" if sandbox else "api.sgroup.qq.com"
    url = f"https://{host}/gateway"
    result = http_json("GET", url, headers={"Authorization": f"QQBot {token}"})
    ws_url = result.get("url")
    if not ws_url:
        raise RuntimeError(f"获取 gateway 失败: {result}")
    print(f"[ok] gateway: {ws_url}  ({'沙箱' if sandbox else '正式'})")
    return ws_url


class MiniWebSocket:
    """极简 WebSocket 客户端（仅文本帧，足够 QQ 网关使用）。"""

    def __init__(self, url: str):
        self.url = url
        self.sock: ssl.SSLSocket | socket.socket | None = None
        self._recv_buf = bytearray()
        self.closed = False

    def connect(self) -> None:
        u = urlparse(self.url)
        if u.scheme not in ("ws", "wss"):
            raise ValueError(f"不支持的 URL: {self.url}")
        host = u.hostname or ""
        port = u.port or (443 if u.scheme == "wss" else 80)
        path = u.path or "/"
        if u.query:
            path = f"{path}?{u.query}"

        raw = socket.create_connection((host, port), timeout=30)
        if u.scheme == "wss":
            ctx = ssl.create_default_context()
            self.sock = ctx.wrap_socket(raw, server_hostname=host)
        else:
            self.sock = raw
        self.sock.settimeout(None)

        key = base64.b64encode(os.urandom(16)).decode("ascii")
        headers = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host}\r\n"
            f"Upgrade: websocket\r\n"
            f"Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            f"Sec-WebSocket-Version: 13\r\n"
            f"\r\n"
        )
        self.sock.sendall(headers.encode("utf-8"))

        # 读 HTTP 响应头
        resp = b""
        while b"\r\n\r\n" not in resp:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionError("WebSocket 握手失败：连接关闭")
            resp += chunk
        head, _, rest = resp.partition(b"\r\n\r\n")
        status_line = head.split(b"\r\n", 1)[0].decode("latin1", errors="replace")
        if "101" not in status_line:
            raise ConnectionError(f"WebSocket 握手失败: {status_line}\n{head.decode('utf-8', errors='replace')}")
        if rest:
            self._recv_buf.extend(rest)

        expected = base64.b64encode(
            hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
        ).decode("ascii")
        accept = ""
        for line in head.decode("latin1", errors="replace").split("\r\n")[1:]:
            if ":" in line:
                k, v = line.split(":", 1)
                if k.strip().lower() == "sec-websocket-accept":
                    accept = v.strip()
        if accept and accept != expected:
            raise ConnectionError("WebSocket 握手校验失败")

    def send_text(self, text: str) -> None:
        if not self.sock or self.closed:
            raise ConnectionError("socket closed")
        payload = text.encode("utf-8")
        frame = bytearray()
        frame.append(0x81)  # FIN + text
        mask_bit = 0x80
        n = len(payload)
        if n < 126:
            frame.append(mask_bit | n)
        elif n < 65536:
            frame.append(mask_bit | 126)
            frame.extend(struct.pack("!H", n))
        else:
            frame.append(mask_bit | 127)
            frame.extend(struct.pack("!Q", n))
        mask = struct.pack("!I", random.getrandbits(32))
        frame.extend(mask)
        frame.extend(bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))
        self.sock.sendall(frame)

    def send_pong(self, data: bytes = b"") -> None:
        if not self.sock or self.closed:
            return
        frame = bytearray([0x8A])
        n = len(data)
        mask_bit = 0x80
        if n < 126:
            frame.append(mask_bit | n)
        else:
            frame.append(mask_bit | 126)
            frame.extend(struct.pack("!H", n))
        mask = struct.pack("!I", random.getrandbits(32))
        frame.extend(mask)
        frame.extend(bytes(b ^ mask[i % 4] for i, b in enumerate(data)))
        self.sock.sendall(frame)

    def _recv_exact(self, n: int) -> bytes:
        while len(self._recv_buf) < n:
            assert self.sock is not None
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionError("连接已关闭")
            self._recv_buf.extend(chunk)
        out = bytes(self._recv_buf[:n])
        del self._recv_buf[:n]
        return out

    def recv_message(self) -> tuple[int, bytes]:
        """返回 (opcode, payload)。"""
        while True:
            b1, b2 = self._recv_exact(2)
            fin = (b1 >> 7) & 1
            opcode = b1 & 0x0F
            masked = (b2 >> 7) & 1
            length = b2 & 0x7F
            if length == 126:
                length = struct.unpack("!H", self._recv_exact(2))[0]
            elif length == 127:
                length = struct.unpack("!Q", self._recv_exact(8))[0]
            mask = self._recv_exact(4) if masked else b""
            payload = self._recv_exact(length)
            if masked:
                payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))

            if opcode == 0x9:  # ping
                self.send_pong(payload)
                continue
            if opcode == 0xA:  # pong
                continue
            if opcode == 0x8:  # close
                self.closed = True
                return opcode, payload
            if not fin:
                # 简单拼接 continuation（QQ 网关一般是完整帧）
                parts = [payload]
                while True:
                    b1, b2 = self._recv_exact(2)
                    op2 = b1 & 0x0F
                    fin2 = (b1 >> 7) & 1
                    masked2 = (b2 >> 7) & 1
                    length2 = b2 & 0x7F
                    if length2 == 126:
                        length2 = struct.unpack("!H", self._recv_exact(2))[0]
                    elif length2 == 127:
                        length2 = struct.unpack("!Q", self._recv_exact(8))[0]
                    mask2 = self._recv_exact(4) if masked2 else b""
                    p2 = self._recv_exact(length2)
                    if masked2:
                        p2 = bytes(b ^ mask2[i % 4] for i, b in enumerate(p2))
                    parts.append(p2)
                    if fin2 or op2 == 0x0:
                        if fin2:
                            break
                return opcode, b"".join(parts)
            return opcode, payload

    def close(self) -> None:
        self.closed = True
        try:
            if self.sock:
                self.sock.close()
        except Exception:
            pass


class OpenIdListener:
    def __init__(self, token: str, ws_url: str):
        self.token = token
        self.ws_url = ws_url
        self.session_id: str | None = None
        self.last_seq: int | None = None
        self.heartbeat_interval_ms = 45000
        self._heartbeat_stop = threading.Event()
        self.found_openids: set[str] = set()
        self.ws: MiniWebSocket | None = None

    def start(self) -> None:
        print()
        print("=" * 60)
        print("已启动监听。请用手机 QQ：")
        print("  1) 扫开放平台「消息列表」相关二维码，添加机器人")
        print("  2) 在消息列表给机器人发任意消息，例如：你好")
        print("收到 C2C_MESSAGE_CREATE 后会打印 user_openid")
        print("按 Ctrl+C 结束")
        print("=" * 60)
        print()

        self.ws = MiniWebSocket(self.ws_url)
        self.ws.connect()
        print("[ws] 已连接，等待 Hello ...")

        try:
            while not self.ws.closed:
                opcode, payload = self.ws.recv_message()
                if opcode == 0x8:
                    print(f"[ws] 对端关闭: {payload!r}")
                    break
                if opcode not in (0x1, 0x2):
                    continue
                text = payload.decode("utf-8", errors="replace")
                self._on_payload(text)
        finally:
            self._heartbeat_stop.set()
            if self.ws:
                self.ws.close()

    def _send(self, obj: dict) -> None:
        assert self.ws is not None
        self.ws.send_text(json.dumps(obj, ensure_ascii=False))

    def _on_payload(self, message: str) -> None:
        try:
            payload = json.loads(message)
        except json.JSONDecodeError:
            print(f"[ws] 非 JSON: {message[:200]}")
            return

        op = payload.get("op")
        t = payload.get("t")
        d = payload.get("d")
        s = payload.get("s")
        if s is not None:
            self.last_seq = s

        if op == 10:
            self.heartbeat_interval_ms = (d or {}).get("heartbeat_interval", 45000)
            print(f"[ws] Hello, heartbeat={self.heartbeat_interval_ms}ms，发送 Identify ...")
            self._send(
                {
                    "op": 2,
                    "d": {
                        "token": f"QQBot {self.token}",
                        "intents": INTENT_GROUP_AND_C2C,
                        "shard": [0, 1],
                        "properties": {
                            "$os": "macos",
                            "$browser": "qq_openid_demo",
                            "$device": "qq_openid_demo",
                        },
                    },
                }
            )
            return

        if op == 0:
            if t == "READY":
                self.session_id = (d or {}).get("session_id")
                user = (d or {}).get("user") or {}
                print(f"[ws] READY session={self.session_id}")
                print(f"[ws] bot user={user.get('username')} id={user.get('id')}")
                self._start_heartbeat()
                return

            if t == "C2C_MESSAGE_CREATE":
                self._handle_c2c(d or {})
                return

            if t in ("FRIEND_ADD", "FRIEND_DEL", "C2C_MSG_RECEIVE", "C2C_MSG_REJECT"):
                print(f"[event] {t}: {json.dumps(d, ensure_ascii=False)}")
                openid = None
                if isinstance(d, dict):
                    openid = d.get("openid") or d.get("user_openid")
                    author = d.get("author") or {}
                    if isinstance(author, dict):
                        openid = openid or author.get("user_openid") or author.get("id")
                if openid:
                    self._report_openid(str(openid), source=t)
                return

            if t:
                print(f"[event] {t}")
            return

        if op == 11:
            return

        if op == 7:
            print("[ws] 服务端要求重连")
            return

        if op == 9:
            print(f"[ws] Invalid Session: {payload}")
            return

        print(f"[ws] op={op} t={t} raw={json.dumps(payload, ensure_ascii=False)[:500]}")

    def _handle_c2c(self, d: dict) -> None:
        author = d.get("author") or {}
        openid = None
        if isinstance(author, dict):
            openid = author.get("user_openid") or author.get("id")
        openid = openid or d.get("openid")
        content = d.get("content", "")
        msg_id = d.get("id", "")
        print()
        print("*" * 60)
        print("收到 C2C_MESSAGE_CREATE（单聊消息）")
        print(f"  content     : {content!r}")
        print(f"  message_id  : {msg_id}")
        print(f"  author 全量 : {json.dumps(author, ensure_ascii=False)}")
        if openid:
            self._report_openid(str(openid), source="C2C_MESSAGE_CREATE")
        else:
            print("  未解析到 openid，完整事件：")
            print(json.dumps(d, ensure_ascii=False, indent=2))
        print("*" * 60)
        print()

    def _report_openid(self, openid: str, source: str) -> None:
        first = openid not in self.found_openids
        self.found_openids.add(openid)
        print(f"  >>> 用户 OpenID ({source}): {openid}")
        if first:
            out = "qq_user_openid.txt"
            with open(out, "a", encoding="utf-8") as f:
                f.write(f"{openid}\t{source}\t{time.strftime('%Y-%m-%d %H:%M:%S')}\n")
            print(f"  >>> 已追加写入 {out}")
            print("  >>> 把上面的 OpenID 填进「消息列表沙箱 / 用户 OpenID」即可")

    def _start_heartbeat(self) -> None:
        self._heartbeat_stop.clear()

        def loop() -> None:
            interval = max(self.heartbeat_interval_ms / 1000.0, 5.0)
            while not self._heartbeat_stop.wait(interval):
                try:
                    self._send({"op": 1, "d": self.last_seq})
                except Exception as e:
                    print(f"[heartbeat] 发送失败: {e}")
                    break

        threading.Thread(target=loop, name="qq-heartbeat", daemon=True).start()
        print("[ws] 心跳已启动")


def main() -> int:
    app_id = APP_ID
    app_secret = APP_SECRET
    if not app_id or not app_secret:
        print("缺少凭证。请设置环境变量后重试：")
        print("  export QQ_APP_ID=你的AppID")
        print("  export QQ_APP_SECRET=你的AppSecret")
        print("  python3 qq_openid_demo.py")
        return 1

    print(f"AppID     : {app_id}")
    print(f"Sandbox   : {USE_SANDBOX}")

    token = get_access_token(app_id, app_secret)
    ws_url = get_gateway(token, USE_SANDBOX)
    listener = OpenIdListener(token, ws_url)
    try:
        listener.start()
    except KeyboardInterrupt:
        print("\n已退出")
    if listener.found_openids:
        print("本次捕获的 OpenID：")
        for oid in listener.found_openids:
            print(f"  {oid}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

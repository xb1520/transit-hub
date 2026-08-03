#!/usr/bin/env python3
"""本地构建 Docker 镜像，经 SSH 上传到远端并用 Compose 启动。

用法:
  python scripts/deploy.py debian@ovh
  python scripts/deploy.py debian@ovh --sudo-docker   # /opt 等需 root 时

参数 user@host 走本机 SSH 配置（~/.ssh/config），无需额外 env 文件。
流程: docker build → docker save | gzip | ssh docker load → 同步 compose → up -d
"""

from __future__ import annotations

import argparse
import atexit
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
COMPOSE_REL = Path("deploy/docker-compose.prod.yml")
DOCKERFILE = Path("deploy/Dockerfile")
DEFAULT_IMAGE = "deviseo/transithub:v0.1.15"
DEFAULT_REMOTE_DIR = "/opt/transit-hub"
HEALTH_PATH = "/api/health"
APP_PORT = 10621

# 当前 SSH 复用连接的 ControlPath（main 里初始化）
_SSH_CONTROL_PATH: str | None = None
_SSH_TARGET: str | None = None


def die(msg: str, code: int = 1) -> None:
    print(f"ERROR: {msg}", file=sys.stderr)
    raise SystemExit(code)


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def shell_quote(s: str) -> str:
    return "'" + s.replace("'", "'\"'\"'") + "'"


def run(cmd: list[str], *, cwd: Path | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    print(f"+ {' '.join(cmd)}", flush=True)
    return subprocess.run(cmd, cwd=cwd, check=check, text=True)


def parse_image_from_compose(compose_path: Path) -> str:
    text = compose_path.read_text(encoding="utf-8")
    m = re.search(r"(?m)^\s*image:\s*(\S+)\s*$", text)
    return m.group(1) if m else DEFAULT_IMAGE


def check_secrets(compose_path: Path, skip: bool) -> None:
    from urllib.parse import quote, unquote, urlparse

    text = compose_path.read_text(encoding="utf-8")
    if not skip and "change-this-" in text:
        die(
            f"{compose_path.relative_to(ROOT)} 仍含 change-this-* 占位符，"
            "请先改好生产密钥。确认后可用 --skip-secret-check 跳过。"
        )

    # DATABASE_URL 密码必须能被正确解析，且解码后等于 POSTGRES_PASSWORD
    url_m = re.search(r"(?m)^\s*DATABASE_URL:\s*(\S+)\s*$", text)
    pg_m = re.search(r"(?m)^\s*POSTGRES_PASSWORD:\s*(\S+)\s*$", text)
    if not url_m or not pg_m:
        return
    url = url_m.group(1)
    pg_pass = pg_m.group(1).strip("\"'")

    # base64 密码常含 + / =，未编码时 Go/pgx 会报 invalid port
    try:
        parsed = urlparse(url)
        if parsed.hostname is None or parsed.password is None:
            raise ValueError("missing host or password")
        # urlparse 会把 + 当空格的是 query；userinfo 中 + 一般保留
        url_pass_decoded = unquote(parsed.password)
    except Exception as e:
        safe = quote(pg_pass, safe="")
        die(
            f"DATABASE_URL 无法解析（{e}）。\n"
            "  密码里的 + / = @ : 等必须在 URL 中百分号编码。\n"
            f"  POSTGRES_PASSWORD 填原始密码；DATABASE_URL 里密码段应类似：\n"
            f"    postgres://transithub:{safe}@postgres:5432/transithub?sslmode=disable"
        )

    if url_pass_decoded != pg_pass:
        safe = quote(pg_pass, safe="")
        die(
            "DATABASE_URL 解码后的密码与 POSTGRES_PASSWORD 不一致。\n"
            "  正确做法：\n"
            "    POSTGRES_PASSWORD: 原始密码（可含 + / =）\n"
            "    DATABASE_URL: 密码段用 URL 编码后的值\n"
            f"  按当前 POSTGRES_PASSWORD，编码后的密码段应为：\n"
            f"    {safe}"
        )

    # 未编码的危险字符（出现在 @ 前的 userinfo 里）
    pass_m = re.match(r"postgres(?:ql)?://[^:/]+:([^@]+)@", url)
    if pass_m:
        raw_seg = pass_m.group(1)
        bad = [c for c in ("/", "@", ":", "?", "#", " ") if c in raw_seg]
        if bad:
            safe = quote(pg_pass, safe="")
            die(
                f"DATABASE_URL 密码段含未编码字符 {bad}，会导致解析失败。\n"
                f"  请改为：postgres://transithub:{safe}@postgres:5432/transithub?sslmode=disable"
            )


def require_local_tools() -> None:
    for name in ("docker", "ssh", "rsync", "gzip"):
        if shutil.which(name) is None:
            die(f"本机缺少命令: {name}")


def ssh_opts() -> list[str]:
    opts = [
        "-o",
        "StrictHostKeyChecking=accept-new",
        "-o",
        "ConnectTimeout=15",
    ]
    if _SSH_CONTROL_PATH:
        opts += [
            "-o",
            "ControlMaster=auto",
            "-o",
            f"ControlPath={_SSH_CONTROL_PATH}",
            "-o",
            "ControlPersist=120",
        ]
    return opts


def ssh_base(target: str) -> list[str]:
    return ["ssh", *ssh_opts(), target]


def rsync_ssh_cmd() -> str:
    # rsync -e 需要一条 shell 可解析的 ssh 命令
    return "ssh " + " ".join(ssh_opts())


def setup_ssh_multiplex(target: str) -> None:
    """建立 SSH 主连接，后续命令复用，密码只输一次。"""
    global _SSH_CONTROL_PATH, _SSH_TARGET
    control_dir = tempfile.mkdtemp(prefix="deploy-ssh-")
    _SSH_CONTROL_PATH = os.path.join(control_dir, "ctl")
    _SSH_TARGET = target

    def _cleanup() -> None:
        if _SSH_CONTROL_PATH and _SSH_TARGET:
            subprocess.run(
                ["ssh", *ssh_opts(), "-O", "exit", _SSH_TARGET],
                check=False,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        shutil.rmtree(control_dir, ignore_errors=True)

    atexit.register(_cleanup)

    log("建立 SSH 连接（后续复用，密码通常只需输入一次）…")
    try:
        run(["ssh", *ssh_opts(), "-o", "ControlMaster=yes", "-N", "-f", target])
    except subprocess.CalledProcessError:
        die(
            f"SSH 连接失败: {target}\n"
            f"  先本机验证: ssh {target}\n"
            f"  若 Permission denied：配置公钥或 IdentityFile。"
        )


def remote_run(
    target: str,
    remote_cmd: str,
    *,
    check: bool = True,
    capture: bool = False,
) -> subprocess.CompletedProcess[str]:
    cmd = ssh_base(target) + [remote_cmd]
    print(f"+ {' '.join(cmd)}", flush=True)
    try:
        return subprocess.run(
            cmd,
            check=check,
            text=True,
            capture_output=capture,
        )
    except subprocess.CalledProcessError as e:
        if e.returncode == 255:
            die(
                f"SSH 连接失败: {target}\n"
                f"  先本机验证: ssh {target}"
            )
        die(f"远端命令失败 (exit {e.returncode}): {remote_cmd}")


def expand_remote_dir(target: str, remote_dir: str) -> str:
    if not remote_dir.startswith("~"):
        return remote_dir.rstrip("/")
    r = remote_run(
        target,
        f"bash -lc {shell_quote(f'printf %s {remote_dir}')}",
        capture=True,
    )
    path = (r.stdout or "").strip()
    if not path:
        die(f"无法展开远端路径: {remote_dir}")
    return path.rstrip("/")


def detect_remote_platform(target: str, docker_bin: str) -> str:
    """探测远端 Docker 架构，返回 docker --platform 值，如 linux/amd64。"""
    # docker info 最准；回退 uname -m
    r = remote_run(
        target,
        f"bash -lc {shell_quote(docker_bin + ' info --format {{.Architecture}}')}",
        capture=True,
        check=False,
    )
    arch = (r.stdout or "").strip().lower() if r.returncode == 0 else ""
    if not arch:
        r = remote_run(
            target,
            "bash -lc 'uname -m'",
            capture=True,
            check=False,
        )
        arch = (r.stdout or "").strip().lower() if r.returncode == 0 else ""

    mapping = {
        "x86_64": "linux/amd64",
        "amd64": "linux/amd64",
        "aarch64": "linux/arm64",
        "arm64": "linux/arm64",
    }
    platform = mapping.get(arch)
    if not platform:
        log(f"无法识别远端架构 ({arch!r})，默认 linux/amd64")
        return "linux/amd64"
    return platform


def docker_build(image: str, platform: str) -> None:
    log(f"本地构建镜像 {image} （platform={platform}）")
    # Mac arm64 部署到 OVH amd64 时必须交叉构建，否则容器无法运行
    run(
        [
            "docker",
            "build",
            "--platform",
            platform,
            "-f",
            str(DOCKERFILE),
            "-t",
            image,
            ".",
        ],
        cwd=ROOT,
    )


def docker_push_via_ssh(target: str, image: str, docker_bin: str) -> None:
    log(f"上传镜像到 {target}（docker save | gzip | ssh load）")
    save = subprocess.Popen(
        ["docker", "save", image],
        stdout=subprocess.PIPE,
        cwd=ROOT,
    )
    gzip_p = subprocess.Popen(
        ["gzip", "-c"],
        stdin=save.stdout,
        stdout=subprocess.PIPE,
    )
    if save.stdout is not None:
        save.stdout.close()

    ssh_cmd = ssh_base(target) + [f"gunzip -c | {docker_bin} load"]
    print(f"+ docker save {image} | gzip -c | {' '.join(ssh_cmd)}", flush=True)
    ssh_p = subprocess.Popen(ssh_cmd, stdin=gzip_p.stdout)
    if gzip_p.stdout is not None:
        gzip_p.stdout.close()

    ssh_rc = ssh_p.wait()
    gzip_rc = gzip_p.wait()
    save_rc = save.wait()
    if save_rc != 0:
        die(f"docker save 失败 (exit {save_rc})")
    if gzip_rc != 0:
        die(f"gzip 失败 (exit {gzip_rc})")
    if ssh_rc != 0:
        die(f"远端 docker load 失败 (exit {ssh_rc})")


def sync_deploy_files(target: str, remote_abs: str, use_sudo: bool) -> None:
    log(f"同步 deploy/ 到 {target}:{remote_abs}/")
    dirs = (
        f"{remote_abs}/deploy",
        f"{remote_abs}/data/postgres",
        f"{remote_abs}/data/redis",
        f"{remote_abs}/data/ticket-uploads",
    )
    mkdir = "sudo mkdir -p" if use_sudo else "mkdir -p"
    remote_run(target, mkdir + " " + " ".join(shell_quote(p) for p in dirs))

    rsync_cmd = [
        "rsync",
        "-az",
        "--human-readable",
        "-e",
        rsync_ssh_cmd(),
    ]
    # /opt 等目录 debian 用户不可写时，用远端 sudo rsync 落盘
    if use_sudo:
        rsync_cmd.append("--rsync-path=sudo rsync")
    rsync_cmd += [
        f"{ROOT / 'deploy'}/",
        f"{target}:{remote_abs}/deploy/",
    ]
    try:
        run(rsync_cmd)
    except subprocess.CalledProcessError as e:
        die(
            f"rsync 失败 (exit {e.returncode})。"
            f"若目标在 /opt 下，请加 --sudo-docker。"
        )


def compose_up(target: str, remote_abs: str, docker_bin: str) -> None:
    log("远端 docker compose up -d")
    cmd = (
        f"cd {shell_quote(remote_abs)} && "
        f"{docker_bin} compose -f {shell_quote(str(COMPOSE_REL))} up -d --remove-orphans && "
        f"{docker_bin} compose -f {shell_quote(str(COMPOSE_REL))} ps"
    )
    remote_run(target, f"bash -lc {shell_quote(cmd)}")


def health_check(
    target: str,
    remote_abs: str,
    docker_bin: str,
    retries: int = 30,
    interval: float = 2.0,
) -> None:
    url = f"http://127.0.0.1:{APP_PORT}{HEALTH_PATH}"
    log(f"健康检查 {url}")
    probe = (
        f"if command -v curl >/dev/null 2>&1; then "
        f"curl -fsS --max-time 5 {shell_quote(url)} >/dev/null; "
        f"elif command -v wget >/dev/null 2>&1; then "
        f"wget -q -O /dev/null --timeout=5 {shell_quote(url)}; "
        f"else exit 2; fi"
    )
    for i in range(1, retries + 1):
        r = remote_run(target, f"bash -lc {shell_quote(probe)}", check=False)
        if r.returncode == 0:
            log(f"健康检查通过（第 {i} 次）")
            return
        time.sleep(interval)
    die(
        "健康检查未通过。查看日志:\n"
        f"  ssh {target} "
        f"'cd {remote_abs} && {docker_bin} compose -f {COMPOSE_REL} logs --tail=100 app'"
    )


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="本地构建 Docker 镜像并 SSH 部署到远端（Compose）",
    )
    p.add_argument(
        "target",
        help="SSH 目标，如 debian@ovh（使用本机 ~/.ssh/config）",
    )
    p.add_argument(
        "--remote-dir",
        default=DEFAULT_REMOTE_DIR,
        help=f"远端项目目录（默认 {DEFAULT_REMOTE_DIR}）",
    )
    p.add_argument(
        "--image",
        default=None,
        help="镜像名:tag（默认读 compose 里 app 的 image）",
    )
    p.add_argument(
        "--no-build",
        action="store_true",
        help="跳过本地 docker build（仍上传本地已有镜像）",
    )
    p.add_argument(
        "--skip-upload",
        action="store_true",
        help="跳过镜像上传（远端已有该镜像时）",
    )
    p.add_argument(
        "--skip-health",
        action="store_true",
        help="跳过健康检查",
    )
    p.add_argument(
        "--skip-secret-check",
        action="store_true",
        help="跳过 change-this-* 占位符检查",
    )
    p.add_argument(
        "--sudo-docker",
        action="store_true",
        help="远端用 sudo 执行 docker，并用 sudo 创建 /opt 目录、rsync 落盘",
    )
    p.add_argument(
        "--platform",
        default=None,
        help="构建目标平台，如 linux/amd64（默认自动探测远端 Docker 架构）",
    )
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv if argv is not None else sys.argv[1:])
    target = args.target
    if "@" not in target:
        die("目标格式应为 user@host，例如: debian@ovh")

    compose_path = ROOT / COMPOSE_REL
    if not compose_path.is_file():
        example = COMPOSE_REL.with_suffix(COMPOSE_REL.suffix + ".example")
        die(
            f"找不到 {COMPOSE_REL}（该文件已 gitignore，含生产密钥）。\n"
            f"  请先: cp {example} {COMPOSE_REL}\n"
            f"  再编辑其中的 change-this-* 密钥后重新部署。"
        )
    if not (ROOT / DOCKERFILE).is_file():
        die(f"找不到 {DOCKERFILE}")

    require_local_tools()
    check_secrets(compose_path, args.skip_secret_check)

    image = args.image or parse_image_from_compose(compose_path)
    use_sudo = bool(args.sudo_docker)
    docker_bin = "sudo docker" if use_sudo else "docker"

    log(f"目标: {target}")
    log(f"镜像: {image}")
    log(f"远端目录: {args.remote_dir}")
    if use_sudo:
        log("权限: 远端 docker / 目录 / rsync 使用 sudo")

    setup_ssh_multiplex(target)

    log("检查远端 Docker…")
    remote_run(
        target,
        f"bash -lc {shell_quote(f'{docker_bin} compose version >/dev/null && {docker_bin} version')}",
    )

    platform = args.platform or detect_remote_platform(target, docker_bin)
    log(f"目标平台: {platform}")

    if not args.no_build:
        docker_build(image, platform)
    else:
        log("跳过本地构建（--no-build）")
        # 仍提示本地镜像架构，避免 arm64 镜像被误传到 amd64
        inspect = subprocess.run(
            [
                "docker",
                "image",
                "inspect",
                image,
                "--format",
                "{{.Os}}/{{.Architecture}}",
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        local_plat = (inspect.stdout or "").strip()
        if local_plat and local_plat != platform:
            die(
                f"本地镜像 {image} 架构为 {local_plat}，远端需要 {platform}。\n"
                f"  请去掉 --no-build 重新交叉构建，或指定 --platform {platform}"
            )

    if not args.skip_upload:
        docker_push_via_ssh(target, image, docker_bin)
    else:
        log("跳过镜像上传（--skip-upload）")

    remote_abs = expand_remote_dir(target, args.remote_dir)
    sync_deploy_files(target, remote_abs, use_sudo=use_sudo)
    compose_up(target, remote_abs, docker_bin)

    if not args.skip_health:
        health_check(target, remote_abs, docker_bin)
    else:
        log("跳过健康检查（--skip-health）")

    host = target.split("@", 1)[1]
    log("部署完成")
    log(f"访问: http://{host}:{APP_PORT}")
    log(
        f"日志: ssh {target} "
        f"'cd {args.remote_dir} && {docker_bin} compose -f {COMPOSE_REL} logs -f app'"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

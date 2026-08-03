#!/usr/bin/env python3
"""本地构建 TransitHub 镜像，并覆盖正在运行的 Docker Compose app 服务。

典型场景：
  - 仓库代码在 A 目录（本脚本所在仓库）
  - 线上/本地 compose 部署在 B 目录（可能是另一份 clone）
  - 不想动 postgres/redis 数据，只替换 app 容器验证本地修复

用法示例：
  # 用当前仓库源码构建，并自动探测正在跑的 deploy 栈，只重建 app
  python3 scripts/local_docker_override.py

  # 指定 compose 项目目录（含 docker-compose.prod.yml 的目录）
  python3 scripts/local_docker_override.py --compose-dir /path/to/transit-hub/deploy

  # 只构建镜像，不重启容器
  python3 scripts/local_docker_override.py build

  # 已有本地镜像，跳过构建直接覆盖
  python3 scripts/local_docker_override.py up --no-build

  # 恢复为 compose 文件里的官方镜像
  python3 scripts/local_docker_override.py restore
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
from pathlib import Path
from typing import Iterable, Sequence


DEFAULT_IMAGE = "transithub:local"
DEFAULT_SERVICE = "app"
DEFAULT_COMPOSE_FILE = "docker-compose.prod.yml"
DEFAULT_OVERRIDE_NAME = "docker-compose.local.override.yml"
DEFAULT_CONTAINER_HINT = "deploy-app-1"


class ScriptError(RuntimeError):
    """可预期的业务错误，打印简洁信息后以非 0 退出。"""


def repo_root() -> Path:
    return Path(__file__).resolve().parent.parent


def host_docker_platform() -> str:
    machine = platform.machine().lower()
    if machine in {"arm64", "aarch64"}:
        return "linux/arm64"
    if machine in {"x86_64", "amd64"}:
        return "linux/amd64"
    # 未知架构时让 Docker 自行决定，不强制 platform。
    return ""


def which_docker() -> str:
    path = shutil.which("docker")
    if not path:
        raise ScriptError("未找到 docker 命令，请先安装 Docker Desktop / Docker Engine，并确保在 PATH 中。")
    return path


def run(
    cmd: Sequence[str],
    *,
    cwd: Path | None = None,
    check: bool = True,
    capture: bool = False,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    printable = " ".join(cmd)
    print(f"\n→ {printable}")
    if cwd is not None:
        print(f"  cwd: {cwd}")
    merged_env = os.environ.copy()
    if env:
        merged_env.update(env)
    result = subprocess.run(
        list(cmd),
        cwd=str(cwd) if cwd else None,
        check=False,
        text=True,
        capture_output=capture,
        env=merged_env,
    )
    if check and result.returncode != 0:
        if capture:
            stderr = (result.stderr or "").strip()
            stdout = (result.stdout or "").strip()
            detail = stderr or stdout or f"exit={result.returncode}"
            raise ScriptError(f"命令失败: {printable}\n{detail}")
        raise ScriptError(f"命令失败 (exit={result.returncode}): {printable}")
    return result


def docker_json(cmd: Sequence[str]) -> object:
    result = run(cmd, check=True, capture=True)
    text = (result.stdout or "").strip()
    if not text:
        return None
    return json.loads(text)


def ensure_compose_plugin(docker_bin: str) -> None:
    result = run([docker_bin, "compose", "version"], check=False, capture=True)
    if result.returncode != 0:
        raise ScriptError("当前 Docker 不支持 `docker compose` 插件，请升级 Docker Desktop / 安装 compose v2。")


def detect_compose_context(
    docker_bin: str,
    *,
    compose_dir: Path | None,
    compose_file: str,
    container_name: str,
    project_name: str | None,
) -> tuple[Path, Path, str]:
    """返回 (compose_dir, compose_file_path, project_name)。"""
    if compose_dir is not None:
        compose_dir = compose_dir.resolve()
        compose_path = compose_dir / compose_file
        if not compose_path.is_file():
            # 允许传入仓库根目录，自动补 deploy/
            alt = compose_dir / "deploy" / compose_file
            if alt.is_file():
                compose_dir = alt.parent
                compose_path = alt
            else:
                raise ScriptError(f"未找到 compose 文件: {compose_path}")
        project = project_name or compose_dir.name
        return compose_dir, compose_path, project

    # 1) 优先从正在运行的 app 容器标签反查
    inspect = run(
        [docker_bin, "inspect", container_name, "--format", "{{json .Config.Labels}}"],
        check=False,
        capture=True,
    )
    if inspect.returncode == 0 and (inspect.stdout or "").strip():
        labels = json.loads(inspect.stdout)
        working_dir = labels.get("com.docker.compose.project.working_dir") or ""
        config_files = labels.get("com.docker.compose.project.config_files") or ""
        project = labels.get("com.docker.compose.project") or project_name or "deploy"
        if working_dir:
            compose_dir_path = Path(working_dir)
            # config_files 可能是逗号分隔的多个文件，取第一个 prod/base
            first_cfg = config_files.split(",")[0].strip() if config_files else ""
            if first_cfg:
                compose_path = Path(first_cfg)
                # 若第一个是 override，尽量找 prod
                for part in config_files.split(","):
                    p = Path(part.strip())
                    if p.name == compose_file or p.name.endswith(".prod.yml"):
                        compose_path = p
                        break
                if not compose_path.is_file() and (compose_dir_path / compose_file).is_file():
                    compose_path = compose_dir_path / compose_file
            else:
                compose_path = compose_dir_path / compose_file
            if compose_path.is_file():
                print(f"已从运行中容器检测到 compose: {compose_path} (project={project})")
                return compose_path.parent, compose_path, project

    # 2) 回退到当前仓库 deploy/
    fallback_dir = repo_root() / "deploy"
    fallback_path = fallback_dir / compose_file
    if fallback_path.is_file():
        project = project_name or fallback_dir.name
        print(f"未检测到运行中容器，使用当前仓库: {fallback_path} (project={project})")
        return fallback_dir, fallback_path, project

    raise ScriptError(
        "无法自动定位 compose 项目。请用 --compose-dir 指定 deploy 目录，"
        f"或确保容器 {container_name} 正在运行。"
    )


def write_override_file(
    path: Path,
    *,
    image: str,
    docker_platform: str,
    set_tz: bool,
) -> None:
    # 用最小 override：只替换镜像，并禁止 pull 覆盖本地 tag。
    # TZ 可选写入，便于日志与系统时间一致；业务日切代码本身已固定 Asia/Shanghai。
    lines = [
        "# 由 scripts/local_docker_override.py 自动生成，请勿手改后提交。",
        "# 与 docker-compose.prod.yml 合并后，仅覆盖 app 镜像为本地构建结果。",
        "services:",
        "  app:",
        f"    image: {image}",
        "    pull_policy: never",
    ]
    if docker_platform:
        lines.append(f"    platform: {docker_platform}")
    if set_tz:
        lines.extend(
            [
                "    environment:",
                "      TZ: Asia/Shanghai",
            ]
        )
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"已写入 override: {path}")


def build_image(
    docker_bin: str,
    *,
    source_root: Path,
    image: str,
    docker_platform: str,
    no_cache: bool,
) -> None:
    dockerfile = source_root / "deploy" / "Dockerfile"
    if not dockerfile.is_file():
        raise ScriptError(f"未找到 Dockerfile: {dockerfile}")

    cmd = [
        docker_bin,
        "build",
        "-f",
        str(dockerfile),
        "-t",
        image,
    ]
    if docker_platform:
        cmd.extend(["--platform", docker_platform])
    if no_cache:
        cmd.append("--no-cache")
    cmd.append(str(source_root))

    print(f"开始构建本地镜像 {image} …（前端+后端多阶段构建，首次可能较慢）")
    run(cmd, check=True)


def compose_cmd(
    docker_bin: str,
    *,
    compose_path: Path,
    override_path: Path | None,
    project: str,
    extra: Iterable[str],
) -> list[str]:
    cmd = [
        docker_bin,
        "compose",
        "-p",
        project,
        "-f",
        str(compose_path),
    ]
    if override_path is not None:
        cmd.extend(["-f", str(override_path)])
    cmd.extend(list(extra))
    return cmd


def deploy_app(
    docker_bin: str,
    *,
    compose_dir: Path,
    compose_path: Path,
    override_path: Path,
    project: str,
    service: str,
) -> None:
    # --no-deps：不重建 postgres/redis
    # --force-recreate：确保换镜像后容器一定重建
    # --remove-orphans：清理旧 app 容器残留
    cmd = compose_cmd(
        docker_bin,
        compose_path=compose_path,
        override_path=override_path,
        project=project,
        extra=["up", "-d", "--no-deps", "--force-recreate", "--remove-orphans", service],
    )
    run(cmd, cwd=compose_dir, check=True)


def restore_app(
    docker_bin: str,
    *,
    compose_dir: Path,
    compose_path: Path,
    override_path: Path,
    project: str,
    service: str,
) -> None:
    # 只用 prod 文件 recreate，不再挂 local override
    if override_path.exists():
        print(f"保留 override 文件供下次使用: {override_path}")
    cmd = compose_cmd(
        docker_bin,
        compose_path=compose_path,
        override_path=None,
        project=project,
        extra=["up", "-d", "--no-deps", "--force-recreate", service],
    )
    run(cmd, cwd=compose_dir, check=True)


def wait_healthy(docker_bin: str, container_name: str, timeout_sec: int = 90) -> None:
    """尽力等待容器起来；没有 healthcheck 时只检查 running。"""
    import time

    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        result = run(
            [
                docker_bin,
                "inspect",
                container_name,
                "--format",
                "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{.Config.Image}}",
            ],
            check=False,
            capture=True,
        )
        if result.returncode == 0:
            status, health, image = (result.stdout or "").strip().split("|", 2)
            if status == "running" and health in {"healthy", "none"}:
                print(f"容器就绪: {container_name} status={status} health={health} image={image}")
                return
            print(f"  等待中… status={status} health={health}")
        time.sleep(2)
    print(f"警告: {timeout_sec}s 内未确认容器就绪，请手动检查: docker logs {container_name}")


def resolve_app_container_name(project: str, service: str) -> str:
    # compose v2 默认命名: {project}-{service}-1
    return f"{project}-{service}-1"


def print_banner(title: str) -> None:
    bar = "=" * 60
    print(f"\n{bar}\n{title}\n{bar}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="本地构建 TransitHub 并覆盖 Docker Compose 中的 app 服务（不动数据库）。",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=textwrap.dedent(
            """\
            子命令:
              up       构建（可选）并用本地镜像覆盖 app（默认）
              build    只构建本地镜像
              restore  恢复为 compose 中配置的官方镜像
              status   显示当前 app 容器镜像与 compose 探测结果

            示例:
              python3 scripts/local_docker_override.py
              python3 scripts/local_docker_override.py --compose-dir /path/to/deploy
              python3 scripts/local_docker_override.py up --no-build
              python3 scripts/local_docker_override.py restore
            """
        ),
    )
    parser.add_argument(
        "command",
        nargs="?",
        default="up",
        choices=["up", "build", "restore", "status"],
        help="操作类型（默认 up）",
    )
    parser.add_argument(
        "--source-root",
        type=Path,
        default=None,
        help="源码仓库根目录（默认：本脚本所在仓库）",
    )
    parser.add_argument(
        "--compose-dir",
        type=Path,
        default=None,
        help="运行中的 compose 目录（含 docker-compose.prod.yml）。默认自动探测。",
    )
    parser.add_argument(
        "--compose-file",
        default=DEFAULT_COMPOSE_FILE,
        help=f"基础 compose 文件名（默认 {DEFAULT_COMPOSE_FILE}）",
    )
    parser.add_argument(
        "--project-name",
        default=None,
        help="docker compose 项目名（默认取 compose 目录名，或运行中容器标签）",
    )
    parser.add_argument(
        "--service",
        default=DEFAULT_SERVICE,
        help=f"要覆盖的服务名（默认 {DEFAULT_SERVICE}）",
    )
    parser.add_argument(
        "--image",
        default=DEFAULT_IMAGE,
        help=f"本地镜像名:tag（默认 {DEFAULT_IMAGE}）",
    )
    parser.add_argument(
        "--container",
        default=DEFAULT_CONTAINER_HINT,
        help=f"用于自动探测的现有 app 容器名（默认 {DEFAULT_CONTAINER_HINT}）",
    )
    parser.add_argument(
        "--platform",
        default=None,
        help="构建/运行平台，如 linux/arm64 或 linux/amd64。默认跟随本机架构。",
    )
    parser.add_argument(
        "--no-build",
        action="store_true",
        help="up 时跳过构建，直接使用已有本地镜像",
    )
    parser.add_argument(
        "--no-cache",
        action="store_true",
        help="docker build --no-cache",
    )
    parser.add_argument(
        "--no-tz",
        action="store_true",
        help="不在 override 中写入 TZ=Asia/Shanghai",
    )
    parser.add_argument(
        "--keep-override",
        action="store_true",
        help="使用 compose 目录内固定 override 文件，而不是临时文件",
    )
    return parser


def cmd_status(docker_bin: str, args: argparse.Namespace) -> None:
    compose_dir, compose_path, project = detect_compose_context(
        docker_bin,
        compose_dir=args.compose_dir,
        compose_file=args.compose_file,
        container_name=args.container,
        project_name=args.project_name,
    )
    container = resolve_app_container_name(project, args.service)
    print(f"compose_dir : {compose_dir}")
    print(f"compose_file: {compose_path}")
    print(f"project     : {project}")
    print(f"service     : {args.service}")
    print(f"container   : {container}")
    print(f"local image : {args.image}")

    result = run(
        [
            docker_bin,
            "inspect",
            container,
            "--format",
            "status={{.State.Status}} image={{.Config.Image}} started={{.State.StartedAt}}",
        ],
        check=False,
        capture=True,
    )
    if result.returncode == 0:
        print(f"running     : {(result.stdout or '').strip()}")
    else:
        print("running     : (容器不存在或未启动)")

    images = run(
        [docker_bin, "images", args.image, "--format", "{{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedSince}}"],
        check=False,
        capture=True,
    )
    local = (images.stdout or "").strip()
    print(f"local built : {local or '(尚未构建)'}")


def cmd_build(docker_bin: str, args: argparse.Namespace) -> None:
    source_root = (args.source_root or repo_root()).resolve()
    docker_platform = args.platform if args.platform is not None else host_docker_platform()
    build_image(
        docker_bin,
        source_root=source_root,
        image=args.image,
        docker_platform=docker_platform,
        no_cache=args.no_cache,
    )
    print(f"\n构建完成: {args.image}")


def cmd_up(docker_bin: str, args: argparse.Namespace) -> None:
    source_root = (args.source_root or repo_root()).resolve()
    docker_platform = args.platform if args.platform is not None else host_docker_platform()

    compose_dir, compose_path, project = detect_compose_context(
        docker_bin,
        compose_dir=args.compose_dir,
        compose_file=args.compose_file,
        container_name=args.container,
        project_name=args.project_name,
    )

    if not args.no_build:
        build_image(
            docker_bin,
            source_root=source_root,
            image=args.image,
            docker_platform=docker_platform,
            no_cache=args.no_cache,
        )
    else:
        # 确认本地镜像存在
        check = run(
            [docker_bin, "image", "inspect", args.image, "--format", "{{.Id}}"],
            check=False,
            capture=True,
        )
        if check.returncode != 0:
            raise ScriptError(f"本地镜像不存在: {args.image}，请先去掉 --no-build 构建，或执行 build 子命令。")

    if args.keep_override:
        override_path = compose_dir / DEFAULT_OVERRIDE_NAME
        write_override_file(
            override_path,
            image=args.image,
            docker_platform=docker_platform,
            set_tz=not args.no_tz,
        )
        deploy_app(
            docker_bin,
            compose_dir=compose_dir,
            compose_path=compose_path,
            override_path=override_path,
            project=project,
            service=args.service,
        )
    else:
        with tempfile.TemporaryDirectory(prefix="transithub-local-override-") as tmp:
            override_path = Path(tmp) / DEFAULT_OVERRIDE_NAME
            write_override_file(
                override_path,
                image=args.image,
                docker_platform=docker_platform,
                set_tz=not args.no_tz,
            )
            deploy_app(
                docker_bin,
                compose_dir=compose_dir,
                compose_path=compose_path,
                override_path=override_path,
                project=project,
                service=args.service,
            )

    container = resolve_app_container_name(project, args.service)
    wait_healthy(docker_bin, container)
    print_banner("覆盖完成")
    print(
        textwrap.dedent(
            f"""\
            project   : {project}
            service   : {args.service}
            image     : {args.image}
            source    : {source_root}
            compose   : {compose_path}
            container : {container}

            验证:
              docker logs -f {container}
              open http://127.0.0.1:10621/

            恢复官方镜像:
              python3 scripts/local_docker_override.py restore --compose-dir {compose_dir}
            """
        ).rstrip()
    )


def cmd_restore(docker_bin: str, args: argparse.Namespace) -> None:
    compose_dir, compose_path, project = detect_compose_context(
        docker_bin,
        compose_dir=args.compose_dir,
        compose_file=args.compose_file,
        container_name=args.container,
        project_name=args.project_name,
    )
    override_path = compose_dir / DEFAULT_OVERRIDE_NAME
    restore_app(
        docker_bin,
        compose_dir=compose_dir,
        compose_path=compose_path,
        override_path=override_path,
        project=project,
        service=args.service,
    )
    container = resolve_app_container_name(project, args.service)
    wait_healthy(docker_bin, container)
    print_banner("已恢复为 compose 配置中的镜像")
    print(f"container: {container}")
    print(f"compose  : {compose_path}")


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)

    try:
        docker_bin = which_docker()
        ensure_compose_plugin(docker_bin)

        if args.command == "status":
            cmd_status(docker_bin, args)
        elif args.command == "build":
            cmd_build(docker_bin, args)
        elif args.command == "restore":
            cmd_restore(docker_bin, args)
        else:
            cmd_up(docker_bin, args)
        return 0
    except ScriptError as exc:
        print(f"\n错误: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\n已取消。", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

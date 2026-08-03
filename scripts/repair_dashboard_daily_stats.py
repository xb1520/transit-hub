#!/usr/bin/env python3
"""回填 / 修复 dashboard_daily_stats 中的历史进货与盈利。

从上游站点历史接口按 Asia/Shanghai 业务日重算：
  - today_purchase = Σ(站点实际消费 × rechargeRate)
  - today_profit   = admin usage/stats（业务日）
  - net_profit     = today_profit - today_purchase
  - site_balance / upstream_balance 保留原快照（时点值，不是按日历史）

依赖本机已运行的 Docker 栈（deploy-postgres-1 / deploy-redis-1）。
跨平台：仅需 Python 3.9+ 与 docker CLI。

示例：
  # 修复昨天与前天
  python3 scripts/repair_dashboard_daily_stats.py --last-days 2

  # 指定日期
  python3 scripts/repair_dashboard_daily_stats.py --dates 2026-07-31,2026-08-01

  # 只预览不写库
  python3 scripts/repair_dashboard_daily_stats.py --last-days 2 --dry-run
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import shutil
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)
BUSINESS_TZ = "Asia/Shanghai"
CST = dt.timezone(dt.timedelta(hours=8))

DEFAULT_PG = "deploy-postgres-1"
DEFAULT_REDIS = "deploy-redis-1"
DEFAULT_PG_USER = "transithub"
DEFAULT_PG_DB = "transithub"


class RepairError(RuntimeError):
    pass


def run(cmd: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, text=True, capture_output=True, check=False)
    if check and result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip()
        raise RepairError(f"命令失败: {' '.join(cmd)}\n{detail}")
    return result


def require_docker() -> str:
    path = shutil.which("docker")
    if not path:
        raise RepairError("未找到 docker，请先安装并确保在 PATH 中。")
    return path


def pg_json(docker: str, pg_container: str, sql: str) -> Any:
    result = run(
        [
            docker,
            "exec",
            pg_container,
            "psql",
            "-U",
            DEFAULT_PG_USER,
            "-d",
            DEFAULT_PG_DB,
            "-t",
            "-A",
            "-c",
            sql,
        ]
    )
    text = (result.stdout or "").strip()
    if not text or text == "null":
        return None
    return json.loads(text)


def pg_exec(docker: str, pg_container: str, sql: str) -> None:
    run(
        [
            docker,
            "exec",
            pg_container,
            "psql",
            "-U",
            DEFAULT_PG_USER,
            "-d",
            DEFAULT_PG_DB,
            "-v",
            "ON_ERROR_STOP=1",
            "-c",
            sql,
        ]
    )


def redis_get(docker: str, redis_container: str, key: str) -> str | None:
    result = run([docker, "exec", redis_container, "redis-cli", "GET", key], check=False)
    text = (result.stdout or "").strip()
    if result.returncode != 0 or not text or text == "(nil)":
        return None
    return text


def redis_keys(docker: str, redis_container: str, pattern: str) -> list[str]:
    result = run([docker, "exec", redis_container, "redis-cli", "KEYS", pattern], check=False)
    if result.returncode != 0:
        return []
    lines = [line.strip() for line in (result.stdout or "").splitlines() if line.strip()]
    return [line for line in lines if line != "(empty array)"]


def http_get_json(url: str, headers: dict[str, str], timeout: float = 45.0) -> dict[str, Any]:
    req_headers = {"User-Agent": UA, "Accept": "application/json", **headers}
    req = urllib.request.Request(url, headers=req_headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")[:300]
        except Exception:
            pass
        raise RepairError(f"HTTP {exc.code} {url}\n{body}") from exc
    except Exception as exc:
        raise RepairError(f"请求失败 {url}: {exc}") from exc


def business_day_range(date_str: str) -> tuple[int, int]:
    day = dt.datetime.strptime(date_str, "%Y-%m-%d").replace(tzinfo=CST)
    start = day
    end = day + dt.timedelta(days=1) - dt.timedelta(seconds=1)
    return int(start.timestamp()), int(end.timestamp())


def load_sites(docker: str, pg_container: str) -> list[dict[str, Any]]:
    data = pg_json(
        docker,
        pg_container,
        "SELECT json_agg(row_to_json(t)) FROM ("
        "SELECT id, name, platform, recharge_rate, admin_account_id, session "
        "FROM upstream_sites ORDER BY name"
        ") t;",
    )
    return list(data or [])


def load_admin_session(docker: str, redis_container: str, admin_account_id: str | None) -> dict[str, Any]:
    keys = redis_keys(docker, redis_container, "dashboard:admin:session:*")
    if not keys:
        raise RepairError("Redis 中没有 dashboard admin session，请先在前端登录仪表盘管理员。")

    candidates: list[dict[str, Any]] = []
    for key in keys:
        raw = redis_get(docker, redis_container, key)
        if not raw:
            continue
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            continue
        # key: dashboard:admin:session:{userID}:{adminAccountID}
        parts = key.split(":")
        account_from_key = parts[-1] if len(parts) >= 5 else ""
        payload["_redis_key"] = key
        payload["_admin_account_id"] = account_from_key
        candidates.append(payload)

    if admin_account_id:
        for item in candidates:
            if item.get("_admin_account_id") == admin_account_id:
                return item
        raise RepairError(f"未找到 admin_account_id={admin_account_id} 的仪表盘会话。")

    if len(candidates) == 1:
        return candidates[0]

    # 多个工作区时，优先匹配站点最多的 admin_account_id
    raise RepairError(
        "存在多个 admin session，请用 --admin-account-id 指定。候选: "
        + ", ".join(sorted({c.get("_admin_account_id", "?") for c in candidates}))
    )


def site_purchase(site: dict[str, Any], date_str: str) -> tuple[float, str]:
    session = site.get("session")
    if not session:
        return 0.0, "no-session"
    rate = float(site.get("recharge_rate") or 0)
    if rate <= 0:
        return 0.0, "rate<=0-skip"
    platform = (site.get("platform") or "").lower()

    if platform == "sub2api":
        base = session.get("BaseURL") or session.get("baseUrl") or ""
        token = session.get("AccessToken") or session.get("accessToken") or ""
        token_type = session.get("TokenType") or session.get("tokenType") or "Bearer"
        if not base or not token:
            return 0.0, "missing-auth"
        url = (
            f"{base}/api/v1/usage/stats?start_date={date_str}&end_date={date_str}"
            f"&timezone={urllib.parse.quote(BUSINESS_TZ)}"
        )
        data = http_get_json(url, {"Authorization": f"{token_type} {token}"})
        payload = data.get("data") or data
        raw = float(payload.get("total_actual_cost") or payload.get("actual_cost") or 0)
        return raw * rate, f"raw={raw:.6f}"

    if platform in {"newapi", "new-api"}:
        base = session.get("BaseURL") or session.get("baseUrl") or ""
        token = session.get("AccessToken") or session.get("accessToken") or ""
        cookie = session.get("Cookie") or session.get("cookie") or ""
        user_id = str(session.get("UserID") or session.get("userId") or "")
        qpu = float(session.get("QuotaPerUnit") or session.get("quotaPerUnit") or 500000) or 500000
        if not base or (not token and not cookie) or not user_id:
            return 0.0, "missing-auth"
        start_ts, end_ts = business_day_range(date_str)
        headers = {"New-Api-User": user_id}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        if cookie:
            headers["Cookie"] = cookie
        url = f"{base}/api/log/self/stat?type=2&start_timestamp={start_ts}&end_timestamp={end_ts}"
        data = http_get_json(url, headers)
        payload = data.get("data") or data
        quota = float((payload or {}).get("quota") or 0)
        raw = quota / qpu
        return raw * rate, f"quota={quota:.0f} raw={raw:.6f}"

    return 0.0, f"unknown-platform:{platform}"


def admin_profit(admin: dict[str, Any], date_str: str) -> float:
    sess = admin.get("session") or {}
    platform = (admin.get("platform") or sess.get("Platform") or sess.get("platform") or "").lower()
    base = admin.get("baseUrl") or sess.get("BaseURL") or sess.get("baseUrl") or ""
    if "sub2api" not in platform:
        raise RepairError(f"暂不支持 admin 平台: {platform or '?'}")

    admin_key = sess.get("AdminAPIKey") or sess.get("adminAPIKey") or ""
    token = sess.get("AccessToken") or sess.get("accessToken") or ""
    token_type = sess.get("TokenType") or sess.get("tokenType") or "Bearer"
    headers: dict[str, str] = {}
    if admin_key:
        headers["x-api-key"] = admin_key
    elif token:
        headers["Authorization"] = f"{token_type} {token}"
    else:
        raise RepairError("admin session 缺少 AdminAPIKey / AccessToken")

    url = (
        f"{base}/api/v1/admin/usage/stats?start_date={date_str}&end_date={date_str}"
        f"&timezone={urllib.parse.quote(BUSINESS_TZ)}"
    )
    data = http_get_json(url, headers)
    payload = data.get("data") or data
    return float(payload.get("total_actual_cost") or payload.get("total_cost") or 0)


def default_dates(last_days: int) -> list[str]:
    """返回不含今天的最近 N 个完整业务日。"""
    today = dt.datetime.now(tz=CST).date()
    return [(today - dt.timedelta(days=i)).isoformat() for i in range(1, last_days + 1)]


def load_existing_snapshot(
    docker: str, pg_container: str, user_id: str, admin_account_id: str, date_str: str
) -> dict[str, Any] | None:
    data = pg_json(
        docker,
        pg_container,
        "SELECT row_to_json(t) FROM ("
        "SELECT date::text, today_profit, today_purchase, net_profit, site_balance, upstream_balance "
        f"FROM dashboard_daily_stats WHERE user_id = '{user_id}' "
        f"AND admin_account_id = '{admin_account_id}' AND date = DATE '{date_str}'"
        ") t;",
    )
    return data


def upsert_snapshot(
    docker: str,
    pg_container: str,
    *,
    user_id: str,
    admin_account_id: str,
    date_str: str,
    today_profit: float,
    today_purchase: float,
    site_balance: float,
    upstream_balance: float,
) -> None:
    net_profit = today_profit - today_purchase
    # 用固定 id 前缀避免依赖 gen_random_uuid 扩展；冲突时更新数值字段。
    snap_id = f"repair_{admin_account_id[-12:]}_{date_str.replace('-', '')}"
    sql = f"""
INSERT INTO dashboard_daily_stats (
  id, user_id, admin_account_id, date,
  today_profit, site_balance, today_purchase, net_profit, upstream_balance, created_at
) VALUES (
  '{snap_id}', '{user_id}', '{admin_account_id}', DATE '{date_str}',
  {today_profit}, {site_balance}, {today_purchase}, {net_profit}, {upstream_balance}, now()
)
ON CONFLICT (user_id, admin_account_id, date) DO UPDATE SET
  today_profit = EXCLUDED.today_profit,
  today_purchase = EXCLUDED.today_purchase,
  net_profit = EXCLUDED.net_profit,
  site_balance = EXCLUDED.site_balance,
  upstream_balance = EXCLUDED.upstream_balance;
"""
    # 唯一索引是 (user_id, admin_account_id, date)，但 ON CONFLICT 需要约束名或列。
    # Postgres 对 unique index 可用 ON CONFLICT (user_id, admin_account_id, date)。
    pg_exec(docker, pg_container, sql)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="修复 dashboard_daily_stats 历史进货/盈利快照")
    parser.add_argument("--dates", help="逗号分隔业务日，如 2026-07-31,2026-08-01")
    parser.add_argument("--last-days", type=int, default=2, help="修复最近 N 个完整业务日（默认 2，不含今天）")
    parser.add_argument("--pg-container", default=DEFAULT_PG)
    parser.add_argument("--redis-container", default=DEFAULT_REDIS)
    parser.add_argument("--admin-account-id", default=None, help="多工作区时指定")
    parser.add_argument("--dry-run", action="store_true", help="只打印，不写库")
    parser.add_argument("--workers", type=int, default=6, help="并发查询站点数")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        docker = require_docker()
        if args.dates:
            dates = [part.strip() for part in args.dates.split(",") if part.strip()]
        else:
            dates = default_dates(args.last_days)
        for date in dates:
            dt.datetime.strptime(date, "%Y-%m-%d")

        sites = load_sites(docker, args.pg_container)
        if not sites:
            raise RepairError("upstream_sites 为空")

        # 推断 workspace：取站点中出现最多的 admin_account_id
        counts: dict[str, int] = {}
        for site in sites:
            aid = site.get("admin_account_id") or ""
            if aid:
                counts[aid] = counts.get(aid, 0) + 1
        inferred_admin = max(counts, key=counts.get) if counts else None
        admin_account_id = args.admin_account_id or inferred_admin
        if not admin_account_id:
            raise RepairError("无法推断 admin_account_id")

        admin = load_admin_session(docker, args.redis_container, admin_account_id)
        # user_id from redis key
        redis_key = admin.get("_redis_key") or ""
        parts = redis_key.split(":")
        user_id = parts[3] if len(parts) >= 5 else ""
        if not user_id:
            raise RepairError(f"无法从 redis key 解析 user_id: {redis_key}")

        workspace_sites = [
            s for s in sites if (s.get("admin_account_id") or "") == admin_account_id
        ]
        print(f"user_id={user_id}")
        print(f"admin_account_id={admin_account_id}")
        print(f"sites={len(workspace_sites)} dates={dates} dry_run={args.dry_run}")

        for date_str in dates:
            print(f"\n==== {date_str} ====")
            existing = load_existing_snapshot(
                docker, args.pg_container, user_id, admin_account_id, date_str
            )
            old_purchase = float((existing or {}).get("today_purchase") or 0)
            old_profit = float((existing or {}).get("today_profit") or 0)

            profit = admin_profit(admin, date_str)
            print(f"profit: {old_profit:.6f} -> {profit:.6f}")

            purchase_total = 0.0
            details: list[tuple[str, float, str]] = []

            def one(site: dict[str, Any]) -> tuple[str, float, str]:
                name = site.get("name") or site.get("id") or "?"
                try:
                    amount, note = site_purchase(site, date_str)
                    return name, amount, note
                except Exception as exc:  # noqa: BLE001 - 单站失败继续汇总
                    return name, 0.0, f"ERROR {exc}"

            with ThreadPoolExecutor(max_workers=max(1, args.workers)) as pool:
                futures = [pool.submit(one, site) for site in workspace_sites]
                for fut in as_completed(futures):
                    name, amount, note = fut.result()
                    purchase_total += amount
                    details.append((name, amount, note))

            details.sort(key=lambda item: item[1], reverse=True)
            for name, amount, note in details:
                print(f"  {name:24} {amount:12.6f}  ({note})")

            print(f"purchase: {old_purchase:.6f} -> {purchase_total:.6f}")
            print(f"net: {profit - purchase_total:.6f}")

            site_balance = float((existing or {}).get("site_balance") or 0)
            upstream_balance = float((existing or {}).get("upstream_balance") or 0)

            if args.dry_run:
                print("dry-run: 未写库")
                continue

            upsert_snapshot(
                docker,
                args.pg_container,
                user_id=user_id,
                admin_account_id=admin_account_id,
                date_str=date_str,
                today_profit=profit,
                today_purchase=purchase_total,
                site_balance=site_balance,
                upstream_balance=upstream_balance,
            )
            print("已写库")

        print("\n完成。刷新仪表盘趋势图即可看到修正后的数据。")
        return 0
    except RepairError as exc:
        print(f"错误: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("已取消", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

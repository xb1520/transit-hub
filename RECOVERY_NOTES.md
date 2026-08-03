# TransitHub 丢失改动恢复说明

## 来源
从本机 `~/.grok/sessions/` 中旧路径
`/Volumes/data/code/work/transit-hub` 下的 3 个 Grok 会话的
`rewind_points.jsonl` 文件快照重建。

会话（2026-08-02 ~ 08-03）：
1. `019fc06a` 上游成本/倍率排序 bug + 财务 MVP
2. `019fc0e4` 进货账本 / 今日进货
3. `019fc1c7` 站点用户 / 双币种 / 标记流水

## 曾本地提交但未 push（磁盘损坏后丢失）
- `6a593d9` fix: correct daily cost metrics and upstream cost priority
- `84d71ee` feat: add credit settlement, dual balance coverage, and subscription assets
- `c1ce6d2` feat: add upstream inbound ledger and split cost vs top-up metrics
其后还有大量未提交改动（站点用户等），一并恢复到快照状态。

## 注意
- 恢复的是会话 rewind 快照中的文件内容，不是 git objects。
- 最后几个 prompt 若未触发新的 rewind，可能差最后一两次微调。
- 请本地编译/跑通后再 commit。

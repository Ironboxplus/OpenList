# 多线程下载：非首片出错不再无限重试，直链过期走刷新

## 现状（已核实，`internal/net/request.go`，基线 `dev/upstream-2026-11`）
- `tryDownloadChunk`（`:437`）：首片以后（`ch.id != 0`），`HttpClient` 返回任何 `HttpStatusCodeError`，都当超载处理，返回 `errInfiniteRetry`（`:491`）。`downloadChunk` 收到后执行 `retry--`（`:409-410`），所以永远不退出。直链过期返回 403 时，这个 worker 每 200–500ms 重发一次，一直持续下去。读取方卡住，不报错。
- 上游 OpenList `op/main` 的这段代码相同，没有修（最近一次改动 `1462d63a` 只保留了取消错误）。
- AList `9703352`（PR #9590 "bound overload retries and fail fast on unrecoverable download errors"）已修：
  - 非首片只有 429/502/503/504 算超载，其余状态码直接返回错误；
  - 超载重试次数上限为 `PartBodyMaxRetries*10`；
  - 退避时长为 `overloadRetry*100ms`，最多 2s，且响应 `ctx.Done()`；
  - `errInfiniteRetry` 改名为 `errOverloadRetry`。
  
  同一个 PR 还把 `SeekableStream.Read` 在 RangeRead 出错时的 `return 0, nil` 改成返回错误。**我们的 `stream.go:290-302` 已经返回 err，这一项不用改。**
- 直链刷新已有现成机制：`GetRangeReaderFromLink`（`internal/stream/util.go:316`）在 `link.Refresher != nil` 时，用 `RefreshableRangeReader` 包住整个下载器。
  - 下载器 `d.cancel(err)` 以后，读取方从 `context.Cause` 拿到的是 `HttpStatusCodeError(403)`，字符串 `"403|Forbidden"`。`IsLinkExpiredError`（`util.go:40`）能认出它（关键词 forbidden/unauthorized）。
  - **但** `selfHealingReadCloser.shouldReconnectAfterRead`（`util.go:236-262`）只在 `ErrUnexpectedEOF`、connection reset、首读 0 字节时重连，不看过期错误。所以链接在读的中途过期，即使修好下载器也只会直接失败，不会刷新。

## 改动（照抄 AList，再补一处刷新）
1. `request.go`：按 AList `9703352` 的 diff 原样改，包括常量、`overloadRetry` 计数、退避和状态码判断。非首片判断状态码用 `HttpStatusCodeError` 的值，因为我们这里拿不到 `resp`，AList 用的是 `resp.StatusCode`。超载状态码集合要和首片的 `switch`（`:446-452`）共用同一个函数 `isOverloadStatus(code int) bool`，不要写两份。
2. 为了让测试能跑完，退避单位做成包级变量 `overloadBackoffUnit = 100 * time.Millisecond`，上限 `overloadBackoffMax = 2 * time.Second`，测试里调小。
3. `util.go` 的 `shouldReconnectAfterRead`：当 `IsLinkExpiredError(err)` 且剩余字节 > 0 时返回 true，走现有的 `reconnectFromCurrentOffsetLocked`，也就是刷新后从当前偏移续读。刷新次数受现有的 `MAX_LINK_REFRESH_COUNT` 限制，不加新的计数。
4. **115 的连接数超限（我旧计划写的 "pmt 3-2"）没有依据**：在仓库、SDK、JOURNAL、过往记录里都没找到 115 返回什么，这一项标为**未定**，不加特判。如果以后抓到真实响应，再把它的状态码加进 `isOverloadStatus`。

## 测试（先写，在旧代码上必须失败；用 httptest，不碰真实网盘）
放在 `internal/net/request_test.go`（以及 `internal/stream` 的相应测试文件），每个测试都加 `t.Deadline` 或 10s 超时保护：
- A：首片 200，其余分片永远 403 → `Download` 返回的 reader 报错，错误里带 403，并且在超时内结束。旧代码这里会卡死，即失败。
- B：其余分片先 503 两次再 200 → 内容完整，sha 一致。
- C：其余分片永远 503 → 有限时间内报错，请求次数 ≤ `PartBodyMaxRetries*10 + 若干`。
- D：`RefreshableRangeReader` 加并发下载器。旧 URL 前 N 字节可读，之后的分片返回 403；Refresher 返回新 URL，新 URL 全部可读 → 读出完整内容、sha 一致，Refresher 正好调用 1 次。旧代码会卡死或失败。
- 现有的 `request_test.go`、`request_cancel_test.go`、stream 包测试都要保持通过。

## 交付
- 提交两个：`fix(net): ...`（request.go 及其测试），`fix(stream): ...`（selfHealing 及其测试）。
- 本机只做 build/vet，测试在 CI 跑。
- 不推送，我审查后再推。

# Retry and timeout defaults

This first batch sets ordinary Raft RPC default budgets to six seconds:
manager Apply, internal Raft Apply, reconciler phase/purge Apply, and local
Raft CLI queries. Explicit caller-selected timeouts remain authoritative.
A timeout does not establish that a write was not committed and does not
introduce automatic retries.

The six-value convention honors Ken Smith, the FreeBSD Release Engineering
Lead before Glen. FreeBSD 6.2 was Glen's first FreeBSD OS.

## Review scope still outstanding

This batch is not the entire timeout inventory. Short dial, status, health,
console, and HTTP-header defaults still need review. Leadership-transfer,
restart-confirmation, graceful-stop, and overall fan-out deadlines need their
per-call and enclosing budgets reviewed together. Security failure limits
are not retry counts and are not relaxed by this convention.

Migration freezes, service restart commands, peer-pinning runs, HTTP idle
connections, and transfer-related deadlines bound operations that can outlast
six seconds. Preserve their operation-specific budgets unless evidence
establishes that six seconds is appropriate. Historical accepted ADRs remain
history and are not rewritten to imply these defaults existed earlier.

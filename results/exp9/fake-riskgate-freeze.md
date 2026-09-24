## fake-riskgate, freeze

6000 calls at 200.0/s, deadline 50 ms. **2881 went through unchecked** ({"breaker_open"=>2842, "timeout"=>39}).

Fault at 10.003s, restart at 20.005s, first checked payment after restart at 20.308s.

Load average (1/5/15 min) before 6.3/9.09/11.58, after 6.8/8.93/11.43.

| phase | calls | unchecked | p50 ms | p99 ms | p99.9 ms | max ms | service p99 ms |
|---|---|---|---|---|---|---|---|
| healthy | 2001 | 415 | 2.569 | 11.251 | 56.772 | 56.979 | 10.212 |
| down_breaker_closed | 15 | 15 | 55.057 | 56.664 | 56.664 | 56.664 | 55.567 |
| breaker_open | 1985 | 1985 | 1.142 | 1.873 | 56.253 | 56.597 | 0.494 |
| recovering | 61 | 60 | 1.211 | 3.433 | 3.433 | 3.433 | 2.539 |
| healthy_after | 1938 | 406 | 2.684 | 59.062 | 116.39 | 120.181 | 15.712 |
| overall | 6000 | 2881 | 1.811 | 25.374 | 111.604 | 120.181 | 14.237 |

Breaker transitions:

- 4.895s closed -> open (5 consecutive failures)
- 6.896s open -> half_open (cool-down elapsed)
- 6.899s half_open -> closed (probe succeeded)
- 10.08s closed -> open (5 consecutive failures)
- 12.081s open -> half_open (cool-down elapsed)
- 12.137s half_open -> open (probe failed)
- 14.142s open -> half_open (cool-down elapsed)
- 14.196s half_open -> open (probe failed)
- 16.201s open -> half_open (cool-down elapsed)
- 16.251s half_open -> open (probe failed)
- 18.251s open -> half_open (cool-down elapsed)
- 18.306s half_open -> open (probe failed)
- 20.306s open -> half_open (cool-down elapsed)
- 20.308s half_open -> closed (probe succeeded)
- 24.685s closed -> open (5 consecutive failures)
- 26.686s open -> half_open (cool-down elapsed)
- 26.688s half_open -> closed (probe succeeded)

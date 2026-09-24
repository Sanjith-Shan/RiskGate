## fake-riskgate, kill

6000 calls at 200.0/s, deadline 50 ms. **2825 went through unchecked** ({"breaker_open"=>2800, "connection"=>10, "timeout"=>15}).

Fault at 10.004s, restart at 20.003s, first checked payment after restart at 22.052s.

Load average (1/5/15 min) before 5.5/9.25/11.72, after 6.3/9.09/11.58.

| phase | calls | unchecked | p50 ms | p99 ms | p99.9 ms | max ms | service p99 ms |
|---|---|---|---|---|---|---|---|
| healthy | 2001 | 1 | 2.734 | 20.954 | 43.085 | 47.754 | 10.362 |
| down_breaker_closed | 4 | 4 | 1.428 | 4.645 | 4.645 | 4.645 | 3.416 |
| breaker_open | 1996 | 1996 | 1.247 | 2.585 | 6.647 | 9.095 | 0.919 |
| recovering | 410 | 409 | 1.179 | 3.427 | 13.47 | 13.47 | 0.879 |
| healthy_after | 1589 | 415 | 2.656 | 19.204 | 56.397 | 57.509 | 13.126 |
| overall | 6000 | 2825 | 1.832 | 10.358 | 53.352 | 57.509 | 8.493 |

Breaker transitions:

- 10.022s closed -> open (5 consecutive failures)
- 12.025s open -> half_open (cool-down elapsed)
- 12.026s half_open -> open (probe failed)
- 14.026s open -> half_open (cool-down elapsed)
- 14.027s half_open -> open (probe failed)
- 16.032s open -> half_open (cool-down elapsed)
- 16.032s half_open -> open (probe failed)
- 18.036s open -> half_open (cool-down elapsed)
- 18.037s half_open -> open (probe failed)
- 20.041s open -> half_open (cool-down elapsed)
- 20.042s half_open -> open (probe failed)
- 22.046s open -> half_open (cool-down elapsed)
- 22.052s half_open -> closed (probe succeeded)
- 24.692s closed -> open (5 consecutive failures)
- 26.696s open -> half_open (cool-down elapsed)
- 26.699s half_open -> closed (probe succeeded)

## riskgate, kill

6000 calls at 200.0/s, deadline 50 ms. **3240 went through unchecked** ({"breaker_open"=>3201, "connection"=>10, "timeout"=>29}).

Fault at 10.0s, restart at 20.002s, first checked payment after restart at 22.057s.

Load average (1/5/15 min) before 6.8/8.93/11.43, after 6.32/8.63/11.24.

| phase | calls | unchecked | p50 ms | p99 ms | p99.9 ms | max ms | service p99 ms |
|---|---|---|---|---|---|---|---|
| healthy | 2000 | 415 | 1.618 | 20.193 | 56.36 | 56.878 | 18.983 |
| down_breaker_closed | 5 | 5 | 1.353 | 4.098 | 4.098 | 4.098 | 3.073 |
| breaker_open | 1996 | 1996 | 1.172 | 2.097 | 5.048 | 6.24 | 0.556 |
| recovering | 411 | 410 | 1.114 | 2.524 | 15.338 | 15.338 | 0.637 |
| healthy_after | 1588 | 414 | 1.563 | 46.39 | 57.175 | 57.258 | 45.392 |
| overall | 6000 | 3240 | 1.374 | 7.363 | 56.581 | 57.258 | 5.878 |

Breaker transitions:

- 6.379s closed -> open (5 consecutive failures)
- 8.381s open -> half_open (cool-down elapsed)
- 8.382s half_open -> closed (probe succeeded)
- 10.021s closed -> open (5 consecutive failures)
- 12.026s open -> half_open (cool-down elapsed)
- 12.027s half_open -> open (probe failed)
- 14.031s open -> half_open (cool-down elapsed)
- 14.032s half_open -> open (probe failed)
- 16.036s open -> half_open (cool-down elapsed)
- 16.037s half_open -> open (probe failed)
- 18.041s open -> half_open (cool-down elapsed)
- 18.042s half_open -> open (probe failed)
- 20.046s open -> half_open (cool-down elapsed)
- 20.047s half_open -> open (probe failed)
- 22.051s open -> half_open (cool-down elapsed)
- 22.057s half_open -> closed (probe succeeded)
- 27.157s closed -> open (5 consecutive failures)
- 29.161s open -> half_open (cool-down elapsed)
- 29.164s half_open -> closed (probe succeeded)

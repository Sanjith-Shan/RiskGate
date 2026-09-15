## riskgate, freeze

6000 calls at 200.0/s, deadline 50 ms. **2063 went through unchecked** ({"breaker_open"=>2043, "timeout"=>20}).

Fault at 10.005s, restart at 20.001s, first checked payment after restart at 20.323s.

Load average (1/5/15 min) before 6.32/8.63/11.24, after 6.16/8.38/11.05.

| phase | calls | unchecked | p50 ms | p99 ms | p99.9 ms | max ms | service p99 ms |
|---|---|---|---|---|---|---|---|
| healthy | 2001 | 0 | 1.651 | 5.562 | 10.985 | 17.519 | 4.095 |
| down_breaker_closed | 16 | 16 | 56.313 | 58.974 | 58.974 | 58.974 | 57.92 |
| breaker_open | 1984 | 1984 | 1.187 | 2.271 | 56.891 | 56.987 | 0.506 |
| recovering | 64 | 63 | 1.115 | 2.848 | 2.848 | 2.848 | 2.114 |
| healthy_after | 1935 | 0 | 1.686 | 5.346 | 10.202 | 13.899 | 3.667 |
| overall | 6000 | 2063 | 1.487 | 5.494 | 56.454 | 58.974 | 4.059 |

Breaker transitions:

- 10.081s closed -> open (5 consecutive failures)
- 12.086s open -> half_open (cool-down elapsed)
- 12.142s half_open -> open (probe failed)
- 14.146s open -> half_open (cool-down elapsed)
- 14.202s half_open -> open (probe failed)
- 16.206s open -> half_open (cool-down elapsed)
- 16.262s half_open -> open (probe failed)
- 18.266s open -> half_open (cool-down elapsed)
- 18.319s half_open -> open (probe failed)
- 20.321s open -> half_open (cool-down elapsed)
- 20.323s half_open -> closed (probe succeeded)

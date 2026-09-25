#### drift

| condition | detail | n | median (ms) | p95 (ms) | min (ms) | max (ms) | timeout | error |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| drift=image;service=web_nginx;events=on | detect | 12 | 77.5 | 102.2 | 50 | 105 | 0 | 1 |
| drift=image;service=web_nginx;events=on | repair | 12 | 410.0 | 509.0 | 300 | 520 | 1 | 0 |

#### scale

| condition | n | median (ms) | p95 (ms) | min (ms) | max (ms) | timeout | error |
| --- | --- | --- | --- | --- | --- | --- | --- |
| scale=10;changes=1;events=on | 12 | 127.5 | 152.2 | 100 | 155 | 1 | 1 |
| scale=50;changes=5;events=off | 12 | 255.0 | 304.5 | 200 | 310 | 1 | 0 |

## linkpulse report

Checked `https://example.com/`: 3 pages crawled, 9 links checked in 1.23s.

| Total | OK | Broken | Redirected | Skipped | Unchecked |
|------:|---:|-------:|-----------:|--------:|----------:|
| 10 | 5 | 4 | 1 | 1 | 0 |

### Broken links (4)

| Status | URL | Found on |
|--------|-----|----------|
| 404 | `https://example.com/blog/old-post` | `https://example.com/blog/`<br>`https://example.com/`<br>`https://example.com/about`<br>and 3 more |
| 500 | `https://example.com/search?q=a\|b` | `https://example.com/` |
| dns: no such host | `https://gone.example.org/` | `https://example.com/blog/` |
| timeout | `https://slow.example.org/report.pdf` | `https://example.com/blog/` |

<details>
<summary>Redirects (1)</summary>

| Status | URL | Target |
|--------|-----|--------|
| 301 | `https://example.com/docs` | `https://docs.example.com/` |

</details>

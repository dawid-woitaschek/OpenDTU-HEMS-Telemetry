Charts use the unmodified Apache ECharts **5.6.0** `dist/echarts.min.js` from
`https://registry.npmjs.org/echarts/-/echarts-5.6.0.tgz`. Its Apache-2.0 license,
NOTICE and bundled d3 license are retained alongside it. No CDN is used at runtime.

The application CSS/JS files are maintained directly, with no production Node
build. `npm ci && npm run check` checks syntax and formatting. `npm run
test:browser` runs Chromium checks against the disposable synthetic preview
(`go run ./scripts/preview`, localhost:8081, preview/preview). This preview never
uses a production database or real credentials. Set `CHROMIUM_PATH` if needed.

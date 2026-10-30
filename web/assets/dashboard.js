/* Snapshot dashboard. Charts never request raw payloads or interpolate nulls. */
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const view = document.body.dataset.view;
  const live = view === "/live";
  const form = $("dashboard-filters");
  const escape = (v) =>
    String(v ?? "").replace(
      /[&<>"']/g,
      (c) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[c],
    );
  const fmt = (v, unit = "", digits = 0) =>
    v == null
      ? "—"
      : `${Number(v).toLocaleString("en-GB", { maximumFractionDigits: digits })}${unit ? " " + unit : ""}`;
  const stamp = (v) =>
    v ? new Date(v).toISOString().replace("T", " ").slice(0, 19) : "—";
  const label = (v) =>
    String(
      v == null
        ? "Unavailable"
        : typeof v === "boolean"
          ? v
            ? "Yes"
            : "No"
          : v,
    )
      .replaceAll("_", " ")
      .replaceAll(".", " › ");
  const badge = (state, caption) =>
    `<span class="badge ${["fresh", "stale", "unknown", "unreachable", "healthy", "disabled", "monitor"].includes(state) ? state : "unknown"}">${escape(caption || label(state[0].toUpperCase() + state.slice(1)))}</span>`;
  const charts = new Map();
  let data,
    timer,
    fetching = false,
    requestNumber = 0,
    activeController;
  function age(capture) {
    const s = (Date.now() - Date.parse(capture)) / 1000;
    if (s < -60) return "Clock is ahead";
    if (s < 60) return "Just captured";
    if (s < 3600) return `${Math.floor(s / 60)}m ago`;
    if (s < 86400)
      return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m ago`;
    return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h ago`;
  }
  function freshness() {
    if (!data?.current) return "unknown";
    const delta =
      (Date.now() - Date.parse(data.current.point.captured_at)) / 1000;
    if (delta < -60 || !data.cadence_seconds) return "unknown";
    return delta > data.cadence_seconds * 2 + 60 ? "stale" : "fresh";
  }
  function restore() {
    const q = new URLSearchParams(location.search);
    if (q.has("sender")) $("sender").value = q.get("sender");
    $("range").value = q.get("range") || "24h";
    for (const key of ["from", "to"])
      if (q.get(key) && !isNaN(Date.parse(q.get(key))))
        $(key).value = new Date(q.get(key)).toISOString().slice(0, 19);
    $("range-label").hidden = live;
    $("custom-dates").hidden = live || $("range").value !== "custom";
  }
  function params() {
    const q = new URLSearchParams({ sender: $("sender").value });
    if (live) {
      // Carry the historical view in navigation URLs, while the live endpoint
      // intentionally ignores it when selecting the newest received capture.
      const previous = new URLSearchParams(location.search);
      for (const key of ["range", "from", "to"])
        if (previous.has(key)) q.set(key, previous.get(key));
    }
    if (!live) {
      q.set("range", $("range").value);
      if ($("range").value === "custom") {
        for (const key of ["from", "to"]) {
          if (!$(key).value)
            throw new Error("Choose both custom dates in UTC.");
          q.set(key, new Date($(key).value + "Z").toISOString());
        }
      }
    }
    return q;
  }
  function updateLinks(q) {
    document.querySelectorAll("nav a").forEach((a) => {
      if (
        ["/overview", "/live", "/hems", "/runtime", "/devices"].includes(
          a.pathname,
        )
      )
        a.search = q.toString();
    });
    document.querySelectorAll("[data-filter-link]").forEach((a) => {
      a.search = q.toString();
    });
    $("all-uploads").search = new URLSearchParams({
      source_id: $("sender").value,
    }).toString();
  }
  async function refresh(push = false) {
    const generation = ++requestNumber;
    activeController?.abort();
    activeController = new AbortController();
    const controller = activeController;
    fetching = false;
    form.querySelector("button").disabled = false;
    let q;
    try {
      q = params();
    } catch (e) {
      showError(e.message);
      return;
    }
    if (!$("sender").value) {
      $("empty").hidden = false;
      $("update-status").textContent = "Create a sender to get started";
      return;
    }
    if (push) history.pushState(null, "", location.pathname + "?" + q);
    else if (!location.search)
      history.replaceState(null, "", location.pathname + "?" + q);
    updateLinks(q);
    fetching = true;
    form.querySelector("button").disabled = true;
    $("update-status").textContent = live
      ? "Checking for new snapshots…"
      : "Loading capture timeline…";
    try {
      const response = await fetch(
        (live ? "/dashboard/live" : "/dashboard/data") + "?" + q,
        {
          credentials: "same-origin",
          headers: { Accept: "application/json" },
          signal: controller.signal,
        },
      );
      if (!response.ok)
        throw new Error(
          response.status === 401
            ? "Authentication expired. Reload to sign in."
            : `Unable to load snapshots (HTTP ${response.status}). Check the selected range and refresh.`,
        );
      const result = await response.json();
      if (generation !== requestNumber) return;
      data = result;
      $("dashboard-error").hidden = true;
      render();
      $("update-status").textContent = live
        ? "● Snapshot-live · checks every 15s"
        : `Updated ${new Date().toISOString().slice(11, 19)} UTC`;
    } catch (e) {
      if (e.name === "AbortError" || generation !== requestNumber) return;
      showError(e.message);
      $("update-status").textContent =
        "Refresh failed · displayed data is last known";
    } finally {
      if (generation === requestNumber) {
        fetching = false;
        form.querySelector("button").disabled = false;
      }
    }
  }
  function showError(message) {
    $("dashboard-error").textContent = message;
    $("dashboard-error").hidden = false;
  }
  function render() {
    $("empty").hidden = !!data.current;
    $("dashboard-content").hidden = !data.current;
    $("dataset-label").textContent = live
      ? "SNAPSHOT-LIVE · newest received snapshot · independent of Overview range"
      : "CAPTURE TIMELINE · current cards use the newest capture inside this range";
    $("dataset-count").textContent =
      `${data.points.length.toLocaleString()} snapshots${data.truncated ? " · latest 10,000 only; narrow the range" : ""}`;
    if (!data.current) return;
    const current = data.current.point;
    const fields = new Map(data.current.fields.map((f) => [f.path, f.value]));
    const get = (path) => fields.get(path) ?? null;
    const overview = view === "/overview" || live;
    $("overview-panels").hidden = !overview;
    $("hems-panels").hidden = view !== "/hems";
    $("runtime-panels").hidden = view !== "/runtime";
    $("devices-panels").hidden = view !== "/devices";
    renderKPIs(current);
    if (overview) {
      renderDecision(current, get);
      renderDevices(get, current);
      renderRecent();
      draw(
        "energy-chart",
        data.points,
        [
          series("Grid", "grid", "red"),
          series("PV", "pv", "yellow"),
          series("Storage", "storage", "green"),
          series("EVSE", "evse", "blue"),
        ],
        { unit: "W" },
      );
      draw(
        "price-chart",
        data.points,
        [
          series("Market", "market_ct", "purple"),
          series("Effective import", "import_ct", "blue"),
        ],
        { unit: "ct/kWh" },
      );
      draw(
        "storage-chart",
        data.points,
        [
          series("SoC", "soc", "green", 0),
          series("Actual power", "storage", "yellow", 1),
          series("Planned", "planned_w", "blue", 1, "bar"),
          series("Shadow if Active", "shadow_w", "purple", 1),
        ],
        { unit: "%", second: "W", soc: true },
      );
      draw("runtime-mini", data.points, memorySeries(), {
        unit: "B",
        second: "B",
        compact: true,
      });
    }
    if (view === "/hems") {
      renderFields(
        "hems-details",
        [
          [
            "Plan & readiness",
            "hems.optimizer.",
            [
              "config.",
              "cost_breakdown.",
              "optimized_cost_nano",
              "baseline_cost_nano",
              "modeled_savings_nano",
              "currency",
              "load_forecast_source",
              "warnings",
              "assumptions",
              "blockers",
              "policy_restrictions",
            ],
          ],
          [
            "Modeled economics & assumptions",
            "hems.optimizer.",
            [],
            [
              "optimized_cost_nano",
              "baseline_cost_nano",
              "modeled_savings_nano",
              "currency",
              "load_forecast_source",
              "warnings",
              "assumptions",
              "blockers",
              "policy_restrictions",
            ],
          ],
          ["Control & readiness", "hems.control."],
          ["Economic dispatch", "hems.economic_dispatch."],
          ["Optimizer configuration", "hems.optimizer.config."],
          ["Modeled cost breakdown", "hems.optimizer.cost_breakdown."],
          ["Constraints & coordination", "hems.constraints."],
          ["Tariff provider", "hems.tariff."],
          ["Forecast provider", "hems.forecast."],
          ["Active preview", "hems.shadow_if_active."],
        ],
        fields,
      );
      renderSchedule();
    }
    if (view === "/runtime") {
      draw(
        "runtime-chart",
        data.points,
        [
          ...memorySeries(),
          series("INTERNAL minimum", "internal_minimum", "amber"),
        ],
        { unit: "B", second: "B" },
      );
      renderFields(
        "runtime-details",
        [
          ["System", "system."],
          ["Diagnostics", "diagnostics."],
          ["Transmission identity", "identity"],
        ],
        fields,
      );
    }
    if (view === "/devices") {
      renderFields(
        "devices-details",
        [
          ["Storage", "storage."],
          ["Wallbox / EVSE", "wallbox."],
          ["Tariff provider", "hems.tariff."],
          ["Forecast provider", "hems.forecast."],
          ["Storage readiness", "hems.control.storage."],
          ["EVSE readiness", "hems.control.evse."],
        ],
        fields,
      );
      $("device-history").innerHTML = data.points
        .slice(-100)
        .reverse()
        .map(
          (p) =>
            `<tr><td><a href="/uploads/${p.id}">${escape(stamp(p.captured_at))}</a></td><td>${badge(p.grid.quality)}</td><td>${badge(p.soc.quality)}</td><td>${fmt(p.soc.value, "%")}</td><td>${badge(p.evse.quality)}</td><td>${fmt(p.evse.value, "W")}</td><td>${badge(p.tariff_quality)}</td></tr>`,
        )
        .join("");
    }
  }
  function renderKPIs(p) {
    const status = freshness();
    const quality = (q) =>
      q === "unknown"
        ? "Unavailable"
        : q === "stale"
          ? "Stale signal"
          : status === "fresh"
            ? "Fresh capture"
            : status === "stale"
              ? "Last known · stale"
              : "Last known · cadence unknown";
    const card = (name, value, unit, description, q, color, icon, stats = "") =>
      `<article class="kpi"><div class="kpi-label">${name}<span class="kpi-icon" aria-hidden="true">${icon}</span></div><div class="kpi-value ${color}">${fmt(value)}<span class="unit">${unit}</span></div><div class="kpi-description">${escape(description)}</div><div class="kpi-age">${badge(q === "unknown" ? "unknown" : q === "stale" ? "stale" : status, q === "unknown" && value != null ? "Unknown quality" : quality(q))} <span data-capture-age>${age(p.captured_at)}</span></div>${stats ? `<div class="kpi-stats">${stats}</div>` : ""}</article>`;
    const gridValues = data.points
      .filter((x) => x.grid.value != null && x.grid.quality === "fresh")
      .map((x) => x.grid.value);
    let stats = "";
    if (gridValues.length)
      stats = `Fresh samples · min ${fmt(Math.min(...gridValues), "W")} / max ${fmt(Math.max(...gridValues), "W")}<br>avg ${fmt(gridValues.reduce((a, b) => a + b, 0) / gridValues.length, "W")}${data.truncated ? " · shown subset" : ""}`;
    $("kpis").innerHTML = [
      card(
        "Grid power",
        p.grid.value,
        "W",
        p.grid.value == null
          ? "Unavailable"
          : p.grid.value > 0
            ? "Import from grid"
            : p.grid.value < 0
              ? "Export to grid"
              : "No net exchange",
        p.grid.quality,
        p.grid.value < 0 ? "export" : "import",
        "ϟ",
        stats,
      ),
      card(
        "Effective import price",
        p.import_ct == null ? null : Math.round(p.import_ct * 100) / 100,
        "ct/kWh",
        "Used by HEMS",
        p.import_ct == null ? "unknown" : p.tariff_quality,
        "price",
        "≋",
      ),
      card(
        "Market price",
        p.market_ct == null ? null : Math.round(p.market_ct * 100) / 100,
        "ct/kWh",
        "Market / day-ahead",
        p.market_ct == null ? "unknown" : p.tariff_quality,
        "market",
        "▥",
      ),
      card(
        "Storage SoC",
        p.soc.value,
        "%",
        "Normalized state of charge",
        p.soc.quality,
        "soc",
        "▣",
      ),
      card(
        "EVSE / Wallbox",
        p.evse.value,
        "W",
        p.evse.value == null
          ? "Unavailable"
          : p.evse.value === 0
            ? "No consumption"
            : "Consumption",
        p.evse.quality,
        "",
        "⌁",
      ),
      `<article class="kpi"><div class="kpi-label">Last snapshot<span class="kpi-icon" aria-hidden="true">◷</span></div><div class="kpi-value">${escape(p.captured_at.slice(11, 19))}</div><div class="kpi-description">${escape(p.captured_at.slice(0, 10))} UTC</div><div class="kpi-age">${badge(status, status === "fresh" ? "Fresh" : status === "stale" ? "Stale · last known" : "Cadence unknown")} <span data-capture-age>${age(p.captured_at)}</span></div><div class="kpi-stats">${data.cadence_seconds ? "Estimated cadence " + fmt(data.cadence_seconds / 60, "min", 1) : "Not enough captures to estimate cadence"}<br>${p.time_basis === "hems_capture" ? "HEMS capture time" : "Legacy receive-time fallback"}</div></article>`,
    ].join("");
    // Retain tariff precision; integer power cards use the same layout helper.
    const values = $("kpis").querySelectorAll(".kpi-value");
    for (const [i, v] of [
      [1, p.import_ct],
      [2, p.market_ct],
    ])
      values[i].innerHTML = `${fmt(v, "", 2)}<span class="unit">ct/kWh</span>`;
  }
  function actionLabel(action) {
    return (
      {
        set_charge_power_w: "Charge",
        set_discharge_power_w: "Discharge",
        set_current_a: "Set current",
        set_target_soc_pct: "Set target SoC",
        stop: "Stop",
        release: "Release",
      }[action] || label(action || "Unavailable")
    );
  }
  function renderDecision(p, get) {
    const mode = get("hems.global_mode");
    $("mode-badge").textContent = mode || "Unknown";
    $("mode-badge").className = "badge monitor";
    const valid = get("hems.shadow_if_active.valid") === true;
    const rows = [
      [
        "Storage / EVSE",
        `${get("hems.control.storage.mode") ?? "—"} / ${get("hems.control.evse.mode") ?? "—"}`,
      ],
      [
        "Plan validity",
        get("hems.optimizer.plan_valid") == null
          ? "Unavailable"
          : get("hems.optimizer.plan_valid")
            ? "Valid"
            : "Invalid",
      ],
      ["Plan revision", get("hems.optimizer.plan_revision")],
      ["Planned battery", fmt(p.planned_w, "W")],
      [
        "Coordinated battery",
        fmt(get("hems.economic_dispatch.coordinated_battery_w"), "W"),
      ],
      [
        "Charge / discharge allowed",
        `${label(get("hems.constraints.applicable.charge_allowed"))} / ${label(get("hems.constraints.applicable.discharge_allowed"))}`,
      ],
    ];
    const blockers = [
      ...[get("hems.optimizer.plan_blocker")].filter(
        (v) => typeof v === "string" && v.length,
      ),
      ...(get("hems.optimizer.blockers") || []),
      ...(get("hems.control.storage.automatic.blockers") || []),
      ...(get("hems.control.evse.automatic.blockers") || []),
    ];
    $("decision").innerHTML =
      `<div class="decision-body"><div class="decision-hero"><div class="eyebrow">SHADOW IF ACTIVE · NO DEVICE WRITES</div><div class="decision-action"><span>${escape(valid ? actionLabel(p.shadow_action) : "Unavailable")}</span><span class="${p.shadow_w < 0 ? "export" : "price"}">${fmt(p.shadow_w, "W")}</span></div><p>${escape(get("hems.shadow_if_active.reason") ?? "No firmware decision reported")}</p></div><dl class="decision-rows">${rows.map(([k, v]) => `<div><dt>${escape(k)}</dt><dd>${escape(v ?? "—")}</dd></div>`).join("")}</dl><h3>CAPTURE TRIGGERS</h3>${p.trigger.length ? p.trigger.map((t) => `<span class="trigger">${escape(label(t))}</span>`).join("") : '<span class="muted">Unavailable</span>'}<h3>READINESS / BLOCKERS</h3>${blockers.length ? [...new Set(blockers)].map((b) => `<div class="reason">${escape(label(b))}</div>`).join("") : "<p>No blockers reported in this snapshot.</p>"}<h3>PROVIDERS</h3><dl class="decision-rows"><div><dt>Tariff planning usable</dt><dd>${escape(label(get("hems.tariff.planning_usable")))}</dd></div><div><dt>Forecast planning usable</dt><dd>${escape(label(get("hems.forecast.planning_usable")))}</dd></div></dl></div>`;
  }
  function deviceStatus(get, prefix) {
    if (get(prefix + ".enabled") === false) return "disabled";
    if (get(prefix + ".reachable") === false) return "unreachable";
    if (
      get(prefix + ".has_data") === false ||
      get(prefix + ".has_update") === false
    )
      return "unknown";
    if (get(prefix + ".stale") === true || get(prefix + ".fresh") === false)
      return "stale";
    if (
      get(prefix + ".fresh") === true ||
      (get(prefix + ".reachable") === true &&
        get(prefix + ".has_update") === true)
    )
      return "fresh";
    return "unknown";
  }
  function renderDevices(get, p) {
    $("device-mini").innerHTML =
      '<div class="device-list">' +
      [
        ["Storage", "storage", fmt(p.soc.value, "%")],
        ["Wallbox / EVSE", "wallbox", fmt(p.evse.value, "W")],
        ["Tariff provider", "hems.tariff", get("hems.tariff.provider") ?? "—"],
        [
          "Forecast provider",
          "hems.forecast",
          get("hems.forecast.provider") ?? "—",
        ],
        [
          "Remote diagnostics",
          "diagnostics",
          fmt(get("diagnostics.allocation_failure_count"), "alloc fails"),
        ],
      ]
        .map(
          ([name, path, detail]) =>
            `<div class="device-row"><span>${name}</span>${badge(path === "diagnostics" ? "unknown" : deviceStatus(get, path), path === "diagnostics" ? "Captured" : undefined)}<small>${escape(detail)}</small></div>`,
        )
        .join("") +
      "</div>";
  }
  function renderRecent() {
    $("recent").innerHTML =
      data.points
        .slice(-8)
        .reverse()
        .map(
          (p) =>
            `<tr><td><a href="/uploads/${p.id}">${escape(stamp(p.captured_at))}</a>${p.time_basis !== "hems_capture" ? "<small>Receive-time fallback</small>" : ""}</td><td>${escape(p.trigger.map(label).join(", ") || "—")}</td><td class="${p.grid.value < 0 ? "export" : "import"}">${fmt(p.grid.value, "W")}</td><td>${fmt(p.import_ct, "ct/kWh", 2)}</td><td>${fmt(p.soc.value, "%")}</td><td>${escape(p.shadow_action ? actionLabel(p.shadow_action) : "Unavailable")}</td><td>${badge("healthy", "Stored")}<small>ACK delivery unknown*</small></td><td>${fmt(p.bytes / 1024, "KiB", 1)}</td></tr>`,
        )
        .join("") +
      '<tr><td colspan="8"><small>* Collector stores accepted snapshots. Device receipt of the ACK and original HTTP status are not recorded.</small></td></tr>';
  }
  function displayField(path, v) {
    if (path === "hems.shadow_if_active.action" && v != null)
      return escape(actionLabel(v));
    if (v == null) return '<span class="muted">— Unavailable</span>';
    if (Array.isArray(v))
      return v.length
        ? v
            .map((x) => `<span class="trigger">${escape(label(x))}</span>`)
            .join("")
        : '<span class="muted">None reported</span>';
    if (typeof v === "boolean")
      return badge(v ? "fresh" : "unknown", v ? "Yes" : "No");
    if (typeof v === "number") {
      if (path.endsWith("_nano"))
        return escape(
          fmt(
            v / 1e9,
            data.current.fields.find(
              (f) => f.path === "hems.optimizer.currency",
            )?.value || "currency units",
            5,
          ),
        );
      if (path.endsWith("_utc"))
        return escape(stamp(new Date(v * 1000).toISOString()));
      return escape(fmt(v, "", 2));
    }
    return escape(label(v));
  }
  function renderFields(id, groups, fields) {
    $(id).innerHTML = groups
      .map(([title, prefix, exclude = [], include = []]) => {
        let rows = [...fields].filter(
          ([k]) =>
            k.startsWith(prefix) &&
            !exclude.some((x) => k.startsWith(prefix + x)) &&
            (!include.length || include.includes(k.slice(prefix.length))),
        );
        if (prefix === "identity") {
          rows = [...fields].filter(([k]) =>
            ["telemetry_sequence", "checkpoint_sequence"].includes(k),
          );
          rows.push(["payload size bytes", data.current.point.bytes]);
          rows.push(["received at UTC", stamp(data.current.point.received_at)]);
        }
        // Keep unavailable fields explicit; all paths are a server-side allowlist.
        return `<section class="panel"><div class="panel-heading"><h2>${escape(title)}</h2></div><div class="fields">${rows.map(([k, v]) => `<div class="field-row"><span class="field-label">${escape(label((prefix === "identity" ? k : k.slice(prefix.length)).replace(/_nano$/, "")))}</span><span class="field-value">${displayField(k, v)}</span></div>`).join("") || "<p>Not reported by this firmware.</p>"}</div></section>`;
      })
      .join("");
  }
  function renderSchedule() {
    const schedule = data.current.schedule;
    $("schedule").innerHTML =
      schedule
        .map(
          (s) =>
            `<tr><td>${stamp(s.start == null ? null : new Date(s.start * 1000).toISOString())} → ${s.end == null ? "—" : new Date(s.end * 1000).toISOString().slice(11, 16)}</td><td>${escape(label(s.action))}</td><td>${fmt(s.battery_w, "W")}</td><td>${fmt(s.end_soc_pct, "%", 1)}</td><td>${fmt(s.pv_w, "W")}</td><td>${fmt(s.assumed_load_w, "W")}</td><td>${fmt(s.import_ct, "ct/kWh", 2)}</td></tr>`,
        )
        .join("") ||
      '<tr><td colspan="7">No future schedule reported.</td></tr>';
    const truncated = data.current.fields.find(
      (f) => f.path === "hems.optimizer.schedule_truncated",
    )?.value;
    $("schedule-note").textContent =
      `Plan captured ${stamp(data.current.point.captured_at)} UTC · ${data.current.point.plan_valid === true ? "valid at capture" : "invalid/cached or validity unavailable"}; modeled values, not measurements.${truncated ? " Firmware truncated the schedule." : ""}`;
    const points = schedule
      .filter((s) => s.start != null)
      .map((s) => ({
        ...s,
        captured_at: new Date(s.start * 1000).toISOString(),
        schedule_end_at:
          s.end == null ? null : new Date(s.end * 1000).toISOString(),
      }));
    draw(
      "schedule-chart",
      points,
      [
        series("Planned battery", "battery_w", "blue", 0, "bar"),
        {
          ...series("End SoC", "end_soc_pct", "green", 1),
          timeKey: "schedule_end_at",
        },
      ],
      { unit: "W", second: "%" },
    );
  }
  function series(name, key, color, axis = 0, type = "line") {
    return { name, key, color, axis, type };
  }
  function memorySeries() {
    return [
      series("INTERNAL free", "internal_free", "blue"),
      series("Largest block", "internal_largest", "purple"),
      series("PSRAM free", "psram_free", "green", 1),
    ];
  }
  function draw(id, points, definitions, options) {
    const el = $(id);
    if (!el || el.closest("[hidden]")) return;
    let chart = charts.get(id);
    if (!chart) {
      chart = echarts.init(el, null, { renderer: "canvas" });
      charts.set(id, chart);
    }
    const css = getComputedStyle(document.documentElement);
    const color = (name) => css.getPropertyValue("--" + name).trim();
    const axis = (unit, index) => ({
      type: "value",
      name: unit,
      nameTextStyle: { color: color("muted"), fontSize: 9 },
      position: index ? "right" : "left",
      axisLabel: {
        color: color("muted"),
        fontSize: 9,
        formatter: (v) =>
          Math.abs(v) >= 1e6
            ? fmt(v / 1e6, "M", 1)
            : Math.abs(v) >= 1000
              ? fmt(v / 1000, "k", 1)
              : fmt(v),
      },
      splitLine: {
        show: !index,
        lineStyle: { color: color("grid"), width: 0.6 },
      },
      axisLine: { show: false },
      axisTick: { show: false },
      ...(options.soc && !index ? { min: 0, max: 100 } : {}),
    });
    chart.setOption(
      {
        animation: false,
        useUTC: true,
        aria: { enabled: true },
        textStyle: { fontFamily: "system-ui,sans-serif" },
        grid: {
          left: 57,
          right: options.second ? 49 : 20,
          top: 28,
          bottom: options.compact ? 49 : 80,
        },
        legend: {
          bottom: options.compact ? 8 : 33,
          textStyle: { color: color("muted"), fontSize: 9 },
          itemWidth: 12,
          itemHeight: 3,
          icon: "roundRect",
        },
        tooltip: {
          trigger: "axis",
          renderMode: "richText",
          backgroundColor: color("panel-hi"),
          borderColor: color("border"),
          textStyle: { color: color("text"), fontSize: 11 },
          confine: true,
          formatter: (items) => {
            if (!items.length) return "";
            const idx = items.find((x) => x.data?.capture)?.data;
            let text = stamp(idx?.capture || items[0].axisValue) + " UTC";
            for (const i of items) {
              const v = Array.isArray(i.value) ? i.value[1] : null;
              text += `\n${i.seriesName}: ${fmt(v, definitions[i.seriesIndex]?.axis === 1 ? options.second : options.unit, 2)}${i.data?.quality ? " · " + i.data.quality : ""}`;
            }
            if (
              items.some(
                (i) => definitions[i.seriesIndex]?.key === "planned_w",
              ) &&
              idx?.planValid !== true
            )
              text += "\nPlan is invalid/cached or validity unavailable";
            if (idx?.received)
              text += "\nReceived " + stamp(idx.received) + " UTC";
            if (idx?.basis === "collector_receive_fallback")
              text += "\nLegacy receive-time fallback";
            return text;
          },
        },
        xAxis: {
          type: "time",
          axisLine: { lineStyle: { color: color("border") } },
          axisTick: { show: false },
          axisLabel: { color: color("muted"), fontSize: 9, hideOverlap: true },
          splitLine: {
            show: true,
            lineStyle: { color: color("grid"), width: 0.5 },
          },
        },
        yAxis: options.second
          ? [axis(options.unit, 0), axis(options.second, 1)]
          : axis(options.unit, 0),
        dataZoom: options.compact
          ? []
          : [
              { type: "inside", filterMode: "none" },
              {
                type: "slider",
                height: 12,
                bottom: 8,
                borderColor: color("border"),
                fillerColor: color("blue-tint"),
                handleStyle: { color: color("blue") },
                textStyle: { color: color("muted"), fontSize: 8 },
                dataBackground: {
                  lineStyle: { color: color("muted") },
                  areaStyle: { opacity: 0 },
                },
              },
            ],
        series: definitions.map((d) => {
          const rows = [];
          let previous;
          for (const p of points) {
            const captureStamp = p[d.timeKey || "captured_at"];
            const capture = Date.parse(captureStamp);
            if (!Number.isFinite(capture)) continue;
            if (
              previous &&
              data.cadence_seconds &&
              capture - previous > (data.cadence_seconds * 2 + 60) * 1000
            )
              rows.push([previous + 1, null]);
            previous = capture;
            const raw = p[d.key];
            const signal = raw !== null && typeof raw === "object" ? raw : null;
            rows.push({
              value: [capture, signal ? signal.value : (raw ?? null)],
              capture: captureStamp,
              received: p.received_at,
              basis: p.time_basis,
              planValid: p.plan_valid,
              quality:
                signal?.quality ||
                (d.key.includes("_ct") ? p.tariff_quality : undefined),
              itemStyle: { opacity: signal?.quality === "stale" ? 0.45 : 1 },
            });
          }
          return {
            name: d.name,
            type: d.type,
            yAxisIndex: d.axis,
            data: rows,
            connectNulls: false,
            smooth: false,
            showSymbol: points.length === 1,
            symbolSize: points.length === 1 ? 9 : 3,
            lineStyle: {
              width: 1.8,
              type: d.key === "shadow_w" ? "dashed" : "solid",
            },
            itemStyle: { color: color(d.color) },
            barMaxWidth: 9,
            emphasis: { focus: "series" },
            areaStyle: d.key === "pv" ? { opacity: 0.07 } : undefined,
          };
        }),
      },
      true,
    );
  }
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    refresh(true);
  });
  $("range").addEventListener("change", () => {
    $("custom-dates").hidden = $("range").value !== "custom";
    if ($("range").value !== "custom") refresh(true);
  });
  $("sender").addEventListener("change", () => refresh(true));
  window.addEventListener("popstate", () => {
    restore();
    refresh();
  });
  window.addEventListener("resize", () => charts.forEach((c) => c.resize()));
  window.addEventListener("themechange", () => {
    if (data) render();
  });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && live) refresh();
  });
  restore();
  refresh();
  function startTimer() {
    clearInterval(timer);
    timer = setInterval(
      () => {
        if (live && !document.hidden && !fetching) refresh();
        else if (data?.current) {
          renderKPIs(data.current.point);
        }
      },
      live ? 15000 : 30000,
    );
  }
  startTimer();
  window.addEventListener("pageshow", (e) => {
    if (e.persisted) {
      startTimer();
      if (live) refresh();
    }
  });
  window.addEventListener("pagehide", () => clearInterval(timer));
})();

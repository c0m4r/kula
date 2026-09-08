/* ============================================================
   chart-accessibility.js — Accessible chart descriptions,
   keyboard point exploration, bounded data tables, and CSV
   alternatives for canvas-rendered time series.
   ============================================================ */
'use strict';

const ENVELOPE_KEY = '$kulaEnvelope';
const TABLE_ROW_LIMIT = 50;

function finiteNumber(value) {
    return typeof value === 'number' && Number.isFinite(value);
}

function timestampValue(value) {
    if (value == null) return NaN;
    const number = value instanceof Date ? value.getTime() : Number(value);
    return Number.isFinite(number) ? number : NaN;
}

function pointTimestamp(point) {
    return timestampValue(point?.x);
}

function pointValue(point) {
    return finiteNumber(point?.y) ? point.y : null;
}

function datasetVisible(chart, index) {
    return typeof chart?.isDatasetVisible === 'function'
        ? chart.isDatasetVisible(index)
        : chart?.data?.datasets?.[index]?.hidden !== true;
}

function visibleDatasets(chart) {
    if (!Array.isArray(chart?.data?.datasets)) return [];
    return chart.data.datasets
        .map((dataset, index) => ({ dataset, index }))
        .filter(({ index }) => datasetVisible(chart, index));
}

function translated(options, key) {
    const value = options?.translate?.(key);
    return value && value !== key ? value : key.replaceAll('_', ' ');
}

function formatTimestamp(options, value) {
    try {
        return options?.formatTimestamp?.(value) || new Date(value).toISOString();
    } catch (_) {
        return new Date(value).toISOString();
    }
}

function formatValue(chart, dataset, value) {
    if (!finiteNumber(value)) return '—';
    const scale = chart?.scales?.[dataset?.yAxisID || 'y'];
    const callback = scale?.options?.ticks?.callback;
    if (typeof callback === 'function') {
        try {
            const formatted = callback.call(scale, value);
            if (formatted != null) return String(formatted);
        } catch (_) { /* use the stable numeric fallback */ }
    }
    return Number.isInteger(value)
        ? String(value)
        : value.toLocaleString(undefined, { maximumFractionDigits: 3 });
}

function lowerBound(values, target) {
    let low = 0;
    let high = values.length;
    while (low < high) {
        const middle = (low + high) >> 1;
        if (values[middle] < target) low = middle + 1;
        else high = middle;
    }
    return low;
}

/** Sorted unique primary observation timestamps inside the active x viewport. */
export function chartTimestamps(chart) {
    const values = new Set();
    const minimum = Number(chart?.scales?.x?.min ?? chart?.options?.scales?.x?.min);
    const maximum = Number(chart?.scales?.x?.max ?? chart?.options?.scales?.x?.max);
    const bounded = Number.isFinite(minimum) && Number.isFinite(maximum) && maximum >= minimum;

    visibleDatasets(chart).forEach(({ dataset }) => {
        (Array.isArray(dataset.data) ? dataset.data : []).forEach(point => {
            const timestamp = pointTimestamp(point);
            if (!Number.isFinite(timestamp) || !finiteNumber(point?.y)) return;
            if (bounded && (timestamp < minimum || timestamp > maximum)) return;
            values.add(timestamp);
        });
    });
    return Array.from(values).sort((a, b) => a - b);
}

/** Resolve a keyboard exploration key to an actual plotted timestamp. */
export function keyboardCursorTimestamp(chart, key, currentTimestamp = null) {
    const timestamps = chartTimestamps(chart);
    if (timestamps.length === 0) return null;
    if (key === 'Home') return timestamps[0];
    if (key === 'End') return timestamps[timestamps.length - 1];

    const current = timestampValue(currentTimestamp);
    if (key === 'Enter' || !Number.isFinite(current)) {
        const minimum = Number(chart?.scales?.x?.min ?? timestamps[0]);
        const maximum = Number(chart?.scales?.x?.max ?? timestamps[timestamps.length - 1]);
        const target = Number.isFinite(minimum) && Number.isFinite(maximum)
            ? (minimum + maximum) / 2
            : timestamps[Math.floor(timestamps.length / 2)];
        const index = lowerBound(timestamps, target);
        if (index === 0) return timestamps[0];
        if (index >= timestamps.length) return timestamps[timestamps.length - 1];
        return target - timestamps[index - 1] <= timestamps[index] - target
            ? timestamps[index - 1]
            : timestamps[index];
    }

    const index = lowerBound(timestamps, current);
    if (key === 'ArrowLeft') {
        return timestamps[Math.max(0, index - 1)];
    }
    if (key === 'ArrowRight') {
        const next = index < timestamps.length && timestamps[index] === current ? index + 1 : index;
        return timestamps[Math.min(timestamps.length - 1, next)];
    }
    return null;
}

function datasetBounds(dataset) {
    const points = Array.isArray(dataset?.data) ? dataset.data : [];
    let first = null;
    let last = null;
    for (let index = 0; index < points.length && first === null; index++) {
        const timestamp = pointTimestamp(points[index]);
        if (Number.isFinite(timestamp)) first = timestamp;
    }
    for (let index = points.length - 1; index >= 0 && last === null; index--) {
        const timestamp = pointTimestamp(points[index]);
        if (Number.isFinite(timestamp)) last = timestamp;
    }
    return { first, last, points: points.length };
}

export function chartDataSummary(chart) {
    const datasets = visibleDatasets(chart);
    let first = Infinity;
    let last = -Infinity;
    let points = 0;
    datasets.forEach(({ dataset }) => {
        const bounds = datasetBounds(dataset);
        if (bounds.first !== null) first = Math.min(first, bounds.first);
        if (bounds.last !== null) last = Math.max(last, bounds.last);
        points = Math.max(points, bounds.points);
    });
    return {
        series: datasets.length,
        points,
        first: first === Infinity ? null : first,
        last: last === -Infinity ? null : last,
        empty: points === 0 || first === Infinity,
    };
}

export function chartSummaryText(chart, options = {}) {
    const summary = chartDataSummary(chart);
    const parts = [translated(options, 'interactive_time_series')];
    if (summary.empty) {
        parts.push(translated(options, 'no_chart_data'));
    } else {
        parts.push(`${summary.series} ${translated(options, 'series')}`);
        parts.push(`${translated(options, 'up_to')} ${summary.points} ${translated(options, 'observations_per_series')}`);
        parts.push(`${translated(options, 'from')} ${formatTimestamp(options, summary.first)} ${translated(options, 'to')} ${formatTimestamp(options, summary.last)}`);
    }
    const status = chart?.$kulaHistoryStatus;
    if (status && status !== 'idle') parts.push(translated(options, `history_${status}`));
    parts.push(translated(options, 'chart_keyboard_help'));
    return parts.join('. ');
}

function hasFiniteEnvelope(dataset, offset) {
    const envelope = dataset?.[ENVELOPE_KEY];
    if (!Array.isArray(envelope)) return false;
    for (let index = offset; index < envelope.length; index += 2) {
        if (finiteNumber(envelope[index])) return true;
    }
    return false;
}

function tableColumns(chart) {
    const columns = [];
    visibleDatasets(chart).forEach(({ dataset, index }) => {
        const label = dataset.label || `Series ${index + 1}`;
        columns.push({ dataset, index, kind: 'value', label });
        if (hasFiniteEnvelope(dataset, 0)) {
            columns.push({ dataset, index, kind: 'minimum', label });
        }
        if (hasFiniteEnvelope(dataset, 1)) {
            columns.push({ dataset, index, kind: 'maximum', label });
        }
    });
    return columns;
}


function seriesValues(column) {
    const values = new Map();

    (column.dataset.data || []).forEach((point, pointIndex) => {
        const timestamp = pointTimestamp(point);
        if (!Number.isFinite(timestamp)) return;
        let value = pointValue(point);
        if (column.kind !== 'value') {
            const offset = column.kind === 'minimum' ? 0 : 1;
            const candidate = column.dataset[ENVELOPE_KEY]?.[pointIndex * 2 + offset];
            value = finiteNumber(candidate) ? candidate : null;
        }
        values.set(timestamp, { value });
    });
    return values;
}

/** Build a latest-N table preview. Passing Infinity yields the full CSV model. */
export function chartTableModel(chart, { limit = TABLE_ROW_LIMIT } = {}) {
    const columns = tableColumns(chart);
    const timestamps = new Set();
    const maps = columns.map(column => {
        const values = seriesValues(column);
        values.forEach((_value, timestamp) => timestamps.add(timestamp));
        return values;
    });
    const minimum = Number(chart?.scales?.x?.min ?? chart?.options?.scales?.x?.min);
    const maximum = Number(chart?.scales?.x?.max ?? chart?.options?.scales?.x?.max);
    const inViewport = timestamp => !Number.isFinite(minimum) || !Number.isFinite(maximum) ||
        (timestamp >= minimum && timestamp <= maximum);

    const ordered = Array.from(timestamps).filter(inViewport).sort((a, b) => a - b);
    const rowLimit = Number.isFinite(limit) && limit >= 0 ? Math.floor(limit) : ordered.length;
    const selected = ordered.slice(Math.max(0, ordered.length - rowLimit));
    return {
        columns: columns.map(column => ({
            label: column.label,
            kind: column.kind,
            datasetIndex: column.index,
            dataset: column.dataset,
        })),
        rows: selected.map(timestamp => ({
            timestamp,
            cells: maps.map(values => values.get(timestamp) || { value: null }),
        })),
        totalRows: ordered.length,
        omittedRows: Math.max(0, ordered.length - selected.length),
    };
}

function columnHeading(column, options) {
    if (column.kind === 'minimum') return `${column.label} — ${translated(options, 'minimum')}`;
    if (column.kind === 'maximum') return `${column.label} — ${translated(options, 'maximum')}`;
    return column.label;
}

function csvCell(value) {
    if (value == null) return '';
    let text = String(value);
    // Spreadsheet applications interpret these leading characters as formulas.
    if (typeof value === 'string' && /^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
}

export function chartCSV(chart, options = {}) {
    const model = chartTableModel(chart, { ...options, limit: Infinity });
    const headers = [translated(options, 'timestamp')];
    model.columns.forEach(column => {
        headers.push(columnHeading(column, options));
    });

    const lines = [headers.map(csvCell).join(',')];
    model.rows.forEach(row => {
        const fields = [formatTimestamp(options, row.timestamp)];
        row.cells.forEach(cell => {
            fields.push(cell.value);
        });
        lines.push(fields.map(csvCell).join(','));
    });
    return `${lines.join('\r\n')}\r\n`;
}

function nearestPoint(points, timestamp) {
    if (!Array.isArray(points) || points.length === 0) return null;
    let low = 0;
    let high = points.length - 1;
    while (low <= high) {
        const middle = (low + high) >> 1;
        const value = pointTimestamp(points[middle]);
        if (value < timestamp) low = middle + 1;
        else if (value > timestamp) high = middle - 1;
        else return points[middle];
    }
    const candidates = [points[high], points[low]].filter(Boolean);
    return candidates.reduce((nearest, point) => {
        if (!nearest) return point;
        return Math.abs(pointTimestamp(point) - timestamp) < Math.abs(pointTimestamp(nearest) - timestamp)
            ? point
            : nearest;
    }, null);
}

export function chartCursorText(chart, timestamp, options = {}) {
    const value = timestampValue(timestamp);
    if (!Number.isFinite(value)) return '';
    const title = chart?.$kulaAccessibility?.title?.textContent || translated(options, 'chart');
    const parts = [`${title}. ${formatTimestamp(options, value)}`];
    const entries = [];
    visibleDatasets(chart).forEach(({ dataset }) => {
        const point = nearestPoint(dataset.data, value);
        if (pointTimestamp(point) === value && finiteNumber(point?.y)) {
            entries.push(`${dataset.label}: ${formatValue(chart, dataset, point.y)}`);
        }
    });
    const maxSeries = 12;
    parts.push(...entries.slice(0, maxSeries));
    if (entries.length > maxSeries) {
        parts.push(`+${entries.length - maxSeries} ${translated(options, 'more_series')}`);
    }
    return parts.join('. ');
}

function renderTable(chart) {
    const access = chart?.$kulaAccessibility;
    if (!access?.panel || access.panel.classList.contains('hidden')) return;
    const options = access.options;
    const model = chartTableModel(chart, options);
    access.tableWrap.replaceChildren();

    access.tableSummary.textContent = model.totalRows === 0
        ? translated(options, 'no_chart_data')
        : `${translated(options, 'showing_latest')} ${model.rows.length} ${translated(options, 'of')} ${model.totalRows} ${translated(options, 'timestamps')}. ${translated(options, 'download_csv_all_data')}`;

    if (model.totalRows === 0) return;
    const table = document.createElement('table');
    table.className = 'chart-data-table';
    const caption = document.createElement('caption');
    caption.textContent = `${access.title.textContent} — ${translated(options, 'chart_data_table')}`;
    table.appendChild(caption);

    const head = document.createElement('thead');
    const headingRow = document.createElement('tr');
    const timeHeading = document.createElement('th');
    timeHeading.scope = 'col';
    timeHeading.textContent = translated(options, 'timestamp');
    headingRow.appendChild(timeHeading);
    model.columns.forEach(column => {
        const heading = document.createElement('th');
        heading.scope = 'col';
        heading.textContent = columnHeading(column, options);
        headingRow.appendChild(heading);
    });
    head.appendChild(headingRow);
    table.appendChild(head);

    const body = document.createElement('tbody');
    model.rows.forEach(row => {
        const tr = document.createElement('tr');
        const timeCell = document.createElement('th');
        timeCell.scope = 'row';
        timeCell.textContent = formatTimestamp(options, row.timestamp);
        tr.appendChild(timeCell);
        row.cells.forEach((cell, index) => {
            const td = document.createElement('td');
            td.textContent = formatValue(chart, model.columns[index].dataset, cell.value);
            tr.appendChild(td);
        });
            body.appendChild(tr);
    });
    table.appendChild(body);
    access.tableWrap.appendChild(table);
}

function updateLabels(chart) {
    const access = chart?.$kulaAccessibility;
    if (!access) return;
    const { options, title, button, downloadButton } = access;
    const chartTitle = title.textContent || translated(options, 'chart');
    if (button) {
        button.textContent = translated(options, 'data_table');
        button.title = `${translated(options, 'view_chart_data')}: ${chartTitle}`;
        button.setAttribute('aria-label', button.title);
    }
    if (downloadButton) {
        downloadButton.textContent = translated(options, 'download_csv');
        downloadButton.title = `${translated(options, 'download_chart_csv')}: ${chartTitle}`;
        downloadButton.setAttribute('aria-label', downloadButton.title);
        access.panel.setAttribute('aria-label', `${chartTitle} — ${translated(options, 'chart_data_table')}`);
    }
    access.card.querySelectorAll('.chart-selector:not([aria-label])').forEach(selector => {
        selector.setAttribute('aria-label', `${chartTitle}: ${translated(options, 'select_series')}`);
    });
}

export function updateChartAccessibility(chart) {
    const access = chart?.$kulaAccessibility;
    if (!access) return;
    syncChartDataControls(chart);
    updateLabels(chart);
    access.summary.textContent = chartSummaryText(chart, access.options);
    renderTable(chart);
}

export function announceChartCursor(chart, timestamp) {
    const access = chart?.$kulaAccessibility;
    if (!access?.cursorStatus) return;
    access.cursorStatus.textContent = Number.isFinite(timestampValue(timestamp))
        ? chartCursorText(chart, timestamp, access.options)
        : translated(access.options, 'pinned_time_cleared');
}

function downloadCSV(chart) {
    const access = chart?.$kulaAccessibility;
    if (!access) return;
    const csv = chartCSV(chart, access.options);
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
    const href = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    const title = access.title.textContent || 'chart';
    const filename = title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'chart';
    anchor.href = href;
    anchor.download = `kula-${filename}.csv`;
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(href), 0);
}

// The preference creates only a Data button. Table/export DOM is allocated
// on first use and discarded on opt-out; basic accessibility stays available.
function syncChartDataControls(chart) {
    const access = chart.$kulaAccessibility;
    if (!access.options.getDataControlsEnabled?.()) {
        if (access.panel?.contains(document.activeElement) || document.activeElement === access.button) {
            chart.canvas.focus();
        }
        access.button?.remove();
        access.panel?.remove();
        access.button = access.panel = access.downloadButton = access.tableWrap = access.tableSummary = null;
        return;
    }
    if (access.button) return;
    let actions = access.card.querySelector('.chart-header-right');
    if (!actions) {
        actions = document.createElement('div');
        actions.className = 'chart-header-right';
        access.card.querySelector('.chart-header')?.appendChild(actions);
    }
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn-chart-data';
    button.setAttribute('aria-expanded', 'false');
    access.button = button;
    actions.appendChild(button);
    button.addEventListener('click', event => {
        event.stopPropagation();
        if (!access.panel) {
            const panel = document.createElement('section');
            panel.id = `${access.baseID}-data-panel`;
            panel.className = 'chart-data-panel hidden';
            const toolbar = document.createElement('div');
            toolbar.className = 'chart-data-toolbar';
            access.tableSummary = document.createElement('p');
            access.tableSummary.className = 'chart-data-summary';
            access.downloadButton = document.createElement('button');
            access.downloadButton.type = 'button';
            access.downloadButton.className = 'chart-data-download';
            access.downloadButton.addEventListener('click', click => {
                click.stopPropagation();
                downloadCSV(chart);
            });
            toolbar.append(access.tableSummary, access.downloadButton);
            access.tableWrap = document.createElement('div');
            access.tableWrap.className = 'chart-data-scroll';
            panel.append(toolbar, access.tableWrap);
            access.card.appendChild(panel);
            access.panel = panel;
            button.setAttribute('aria-controls', panel.id);
            updateLabels(chart);
        }
        const open = !access.panel.classList.toggle('hidden');
        button.setAttribute('aria-expanded', String(open));
        if (open) renderTable(chart);
    });
}

/** Attach DOM alternatives and point navigation to one Chart.js instance. */
export function attachChartAccessibility(chart, options = {}) {
    const canvas = chart?.canvas;
    const card = canvas?.closest?.('.chart-card');
    const body = canvas?.closest?.('.chart-body');
    const title = card?.querySelector?.('.chart-header h3');
    if (!canvas || !card || !body || !title) return () => {};

    const baseID = canvas.id || `kula-chart-${Math.random().toString(36).slice(2)}`;
    if (!title.id) title.id = `${baseID}-title`;
    const summary = document.createElement('p');
    summary.id = `${baseID}-summary`;
    summary.className = 'sr-only chart-accessible-summary';
    const cursorStatus = document.createElement('p');
    cursorStatus.id = `${baseID}-cursor-status`;
    cursorStatus.className = 'sr-only';
    cursorStatus.setAttribute('role', 'status');
    cursorStatus.setAttribute('aria-live', 'polite');
    cursorStatus.setAttribute('aria-atomic', 'true');
    body.append(summary, cursorStatus);

    canvas.tabIndex = canvas.tabIndex >= 0 ? canvas.tabIndex : 0;
    canvas.setAttribute('role', 'img');
    canvas.setAttribute('aria-labelledby', title.id);
    canvas.setAttribute('aria-describedby', summary.id);
    canvas.setAttribute('aria-keyshortcuts', 'Enter ArrowLeft ArrowRight Home End Escape');
    canvas.textContent = translated(options, 'chart_canvas_fallback');

    chart.$kulaAccessibility = { card, title, summary, cursorStatus, options, baseID };

    const keyDown = event => {
        const pinned = timestampValue(options.getPinnedTimestamp?.());
        let action = null;
        let timestamp = null;
        if (event.key === 'Enter') {
            if (Number.isFinite(pinned)) action = 'unpin';
            else {
                timestamp = keyboardCursorTimestamp(chart, 'Enter');
                if (timestamp !== null) action = 'pin';
            }
        } else if (event.key === 'Escape' && Number.isFinite(pinned)) {
            action = 'unpin';
        } else if (event.key === 'Home' || event.key === 'End') {
            timestamp = keyboardCursorTimestamp(chart, event.key, pinned);
            if (timestamp !== null) action = Number.isFinite(pinned) ? 'move' : 'pin';
        } else if ((event.key === 'ArrowLeft' || event.key === 'ArrowRight') && Number.isFinite(pinned)) {
            timestamp = keyboardCursorTimestamp(chart, event.key, pinned);
            if (timestamp !== null) action = 'move';
        }
        if (!action) return;
        event.preventDefault();
        event.stopImmediatePropagation();
        document.dispatchEvent(new CustomEvent('kula-crosshair', {
            detail: { action, timestamp, chart, keyboard: true },
        }));
    };
    const focus = () => updateChartAccessibility(chart);

    canvas.addEventListener('keydown', keyDown, true);
    canvas.addEventListener('focus', focus);
    updateChartAccessibility(chart);

    return () => {
        canvas.removeEventListener('keydown', keyDown, true);
        canvas.removeEventListener('focus', focus);
        chart.$kulaAccessibility?.button?.remove();
        chart.$kulaAccessibility?.panel?.remove();
        summary.remove();
        cursorStatus.remove();
        delete chart.$kulaAccessibility;
    };
}

export const chartAccessibilityPlugin = {
    id: 'kulaAccessibility',
    afterUpdate(chart) {
        updateChartAccessibility(chart);
    },
};

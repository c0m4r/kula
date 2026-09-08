/* ============================================================
   chart-controller.js — Chart registration, animation-frame
   batching, viewport culling, and plot-width based point budgets.
   ============================================================ */
'use strict';

function defaultScheduleFrame(callback) {
    if (typeof requestAnimationFrame === 'function') {
        return requestAnimationFrame(callback);
    }
    return setTimeout(callback, 0);
}

function defaultCancelFrame(handle) {
    if (typeof cancelAnimationFrame === 'function') {
        cancelAnimationFrame(handle);
    } else {
        clearTimeout(handle);
    }
}

function chartTarget(chart) {
    return chart?.canvas?.closest?.('.chart-card') || chart?.canvas || null;
}

function targetIsRenderable(target) {
    if (!target) return true;
    if (target.classList?.contains('hidden') || target.classList?.contains('chart-search-hidden')) {
        return false;
    }

    // IntersectionObserver is authoritative once it has reported. This
    // synchronous check handles newly-created cards before the first report.
    if (typeof target.getBoundingClientRect !== 'function' || typeof window === 'undefined') {
        return true;
    }
    const rect = target.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) return false;
    return rect.bottom >= 0 && rect.right >= 0 &&
        rect.top <= window.innerHeight && rect.left <= window.innerWidth;
}

export class ChartUpdateController {
    constructor({ scheduleFrame = defaultScheduleFrame, cancelFrame = defaultCancelFrame, observerFactory } = {}) {
        this.scheduleFrame = scheduleFrame;
        this.cancelFrame = cancelFrame;
        this.charts = new Set();
        // A data/scale update subsumes a render-only invalidation. Keeping the
        // strongest pending mode per chart lets cursor movement repaint via
        // render() without making live-data updates skip scale calculation.
        this.dirty = new Map();
        this.visible = new WeakMap();
        this.targetCharts = new Map();
        this.frame = null;

        const factory = observerFactory || (typeof IntersectionObserver === 'function'
            // Match targetIsRenderable's strict viewport boundary. A positive
            // root margin can report an off-screen chart as intersecting before
            // it is renderable and may not emit another threshold transition
            // when the card crosses the actual viewport edge.
            ? callback => new IntersectionObserver(callback, { rootMargin: '0px' })
            : null);
        this.observer = factory ? factory(entries => this.onIntersections(entries)) : null;
    }

    register(chart, cleanup = null) {
        if (!chart || this.charts.has(chart)) return chart;

        this.charts.add(chart);
        chart.$kulaCleanup = cleanup;
        const target = chartTarget(chart);
        if (target) {
            this.targetCharts.set(target, chart);
            this.visible.set(chart, targetIsRenderable(target));
            this.observer?.observe(target);
        } else {
            this.visible.set(chart, true);
        }

        if (!chart.$kulaDestroyWrapped && typeof chart.destroy === 'function') {
            const destroy = chart.destroy.bind(chart);
            chart.destroy = (...args) => {
                this.unregister(chart);
                return destroy(...args);
            };
            chart.$kulaDestroyWrapped = true;
        }
        return chart;
    }

    unregister(chart) {
        if (!chart || !this.charts.delete(chart)) return;
        this.dirty.delete(chart);
        const target = chartTarget(chart);
        if (target) {
            this.observer?.unobserve(target);
            this.targetCharts.delete(target);
        }
        if (typeof chart.$kulaCleanup === 'function') chart.$kulaCleanup();
        chart.$kulaCleanup = null;
    }

    onIntersections(entries) {
        let becameVisible = false;
        entries.forEach(entry => {
            const chart = this.targetCharts.get(entry.target);
            if (!chart) return;
            const visible = entry.isIntersecting && targetIsRenderable(entry.target);
            this.visible.set(chart, visible);
            if (visible && this.dirty.has(chart)) becameVisible = true;
        });
        if (becameVisible) this.requestFlush();
    }

    isVisible(chart) {
        const target = chartTarget(chart);
        if (!targetIsRenderable(target)) return false;
        return this.visible.get(chart) !== false;
    }

    mark(chart, mode = 'update') {
        if (!chart || !this.charts.has(chart)) return;
        const nextMode = mode === 'render' ? 'render' : 'update';
        if (this.dirty.get(chart) !== 'update') {
            this.dirty.set(chart, nextMode);
        }
        this.requestFlush();
    }

    markAll(charts = this.charts, mode = 'update') {
        for (const chart of charts) {
            if (!chart || !this.charts.has(chart)) continue;
            const nextMode = mode === 'render' ? 'render' : 'update';
            if (this.dirty.get(chart) !== 'update') {
                this.dirty.set(chart, nextMode);
            }
        }
        this.requestFlush();
    }

    requestFlush() {
        if (this.frame !== null || this.dirty.size === 0) return;
        this.frame = this.scheduleFrame(() => {
            this.frame = null;
            this.flush();
        });
    }

    flush() {
        const visible = [];

        // Resolve every layout-dependent visibility read before Chart.js can
        // write canvas/layout state. This avoids read-after-write layout churn
        // when many cards become dirty in the same frame.
        for (const [chart, mode] of Array.from(this.dirty.entries())) {
            if (!this.charts.has(chart)) {
                this.dirty.delete(chart);
                continue;
            }
            if (!this.isVisible(chart)) continue;
            visible.push([chart, mode]);
        }

        for (const [chart, mode] of visible) {
            this.dirty.delete(chart);
            if (mode === 'render' && typeof chart.render === 'function') {
                chart.render();
            } else if (typeof chart.update === 'function') {
                chart.update('none');
            }
        }
    }

    clear() {
        if (this.frame !== null) this.cancelFrame(this.frame);
        this.frame = null;
        for (const chart of Array.from(this.charts)) this.unregister(chart);
        this.dirty.clear();
    }

    forEach(callback) {
        this.charts.forEach(callback);
    }

    plotWidth(fallback = 1000) {
        let width = 0;
        this.charts.forEach(chart => {
            if (!this.isVisible(chart)) return;
            const candidate = chart.chartArea?.width || chart.canvas?.clientWidth ||
                chartTarget(chart)?.getBoundingClientRect?.().width || 0;
            if (Number.isFinite(candidate)) width = Math.max(width, candidate);
        });
        return width > 0 ? width : fallback;
    }
}

export const chartUpdates = new ChartUpdateController();

export function registerChart(chart, cleanup) {
    return chartUpdates.register(chart, cleanup);
}

export function queueChartUpdate(chart) {
    chartUpdates.mark(chart);
}

export function queueAllChartUpdates() {
    chartUpdates.markAll();
}

export function queueAllChartRenders() {
    chartUpdates.markAll(chartUpdates.charts, 'render');
}

export function forEachRegisteredChart(callback) {
    chartUpdates.forEach(callback);
}

export function setSharedCrosshair(timestamp) {
    chartUpdates.forEach(chart => {
        chart.$kulaCrosshairTimestamp = Number.isFinite(timestamp) ? timestamp : null;
    });
    queueAllChartRenders();
}

export function historyPointBudget() {
    // One source point per CSS pixel is already denser than the rendered line.
    // Keep a useful floor for narrow/mobile cards and honor the API's 5,000 cap.
    return Math.max(300, Math.min(5000, Math.ceil(chartUpdates.plotWidth())));
}

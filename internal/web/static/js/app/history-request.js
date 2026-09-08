/* ============================================================
   history-request.js — Latest-request-wins coordination for
   every history API request.
   ============================================================ */
'use strict';

export class HistoryRequestController {
    constructor(fetchImpl = (...args) => fetch(...args)) {
        this.fetchImpl = fetchImpl;
        this.generation = 0;
        this.active = null;
    }

    isCurrent(request) {
        return this.active === request && this.generation === request.generation;
    }

    // Supersede also represents a viewport that can be served locally. The
    // generation advances even when there is no network request, so a stale
    // response can never replace a newer local redraw.
    supersede() {
        this.generation++;
        if (this.active) {
            this.active.controller.abort();
            this.active = null;
        }
        return this.generation;
    }

    async fetchJSON(url, { onStart, onApply, onFailure, onFinish } = {}) {
        const generation = this.supersede();
        const controller = new AbortController();
        const request = { generation, controller };
        this.active = request;

        try {
            onStart?.(generation);

            const response = await this.fetchImpl(url, { signal: controller.signal });
            if (!response.ok) {
                const suffix = response.status ? ` (${response.status})` : '';
                throw new Error(`History request failed${suffix}`);
            }

            const payload = await response.json();
            if (!this.isCurrent(request)) {
                return { status: 'superseded', generation };
            }

            onApply?.(payload, {
                generation,
                isCurrent: () => this.isCurrent(request),
            });
            if (!this.isCurrent(request)) {
                return { status: 'superseded', generation };
            }
            return { status: 'applied', generation };
        } catch (error) {
            if (controller.signal.aborted || !this.isCurrent(request) || error?.name === 'AbortError') {
                return { status: 'superseded', generation };
            }

            onFailure?.(error, generation);
            return { status: 'failed', generation, error };
        } finally {
            if (this.isCurrent(request)) {
                this.active = null;
                onFinish?.(generation);
            }
        }
    }
}

// Long rolling windows are refreshed as complete bounded snapshots. Short
// windows continue using the live stream when observations fit in the budget.
export function liveHistoryRefreshInterval(rangeSeconds, points, sampleIntervalMs = 1000) {
    const step = rangeSeconds * 1000 / Math.max(1, points - 1);
    const observedInterval = Number.isFinite(sampleIntervalMs) && sampleIntervalMs > 0
        ? sampleIntervalMs
        : 1000;
    return step > observedInterval * 1.05 ? Math.max(1000, Math.ceil(step)) : 0;
}

// Keep a short robust window rather than remembering the smallest interval
// forever. One early/late tick must not turn a supported 5s collector into a
// permanent 1s estimate (or vice versa).
export function updateLiveSampleInterval(intervals, deltaMs, limit = 9) {
    const previous = Array.isArray(intervals)
        ? intervals.filter(value => Number.isFinite(value) && value > 0)
        : [];
    if (!Number.isFinite(deltaMs) || deltaMs <= 0) {
        const sorted = [...previous].sort((a, b) => a - b);
        return {
            intervals: previous.slice(-Math.max(1, limit)),
            estimate: sorted.length > 0 ? sorted[Math.floor(sorted.length / 2)] : null,
        };
    }

    const boundedLimit = Math.max(1, Math.trunc(limit) || 1);
    const next = [...previous, deltaMs].slice(-boundedLimit);
    const sorted = [...next].sort((a, b) => a - b);
    return { intervals: next, estimate: sorted[Math.floor(sorted.length / 2)] };
}

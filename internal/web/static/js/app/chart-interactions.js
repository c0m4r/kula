/* ============================================================
   chart-interactions.js — Pointer and keyboard navigation,
   shared crosshairs, and tooltip behavior for time-series charts.
   ============================================================ */
'use strict';

export function panTimeRange(min, max, deltaPixels, plotWidth) {
    const span = max - min;
    if (!Number.isFinite(span) || span <= 0 || !Number.isFinite(plotWidth) || plotWidth <= 0) {
        return { min, max };
    }
    const shift = -(deltaPixels / plotWidth) * span;
    return { min: min + shift, max: max + shift };
}

export function zoomTimeRange(min, max, factor, anchor = 0.5) {
    const span = max - min;
    if (!Number.isFinite(span) || span <= 0 || !Number.isFinite(factor) || factor <= 0) {
        return { min, max };
    }
    const clampedAnchor = Math.max(0, Math.min(1, anchor));
    const pivot = min + span * clampedAnchor;
    const nextSpan = span * factor;
    return {
        min: pivot - nextSpan * clampedAnchor,
        max: pivot + nextSpan * (1 - clampedAnchor),
    };
}

export class CrosshairState {
    constructor() {
        this.hovered = null;
        this.pinned = null;
    }

    apply(action, timestamp = null) {
        if (action === 'pin') {
            if (this.pinned !== null) {
                this.pinned = null;
            } else if (Number.isFinite(timestamp)) {
                this.pinned = timestamp;
            }
        } else if (action === 'move' && Number.isFinite(timestamp)) {
            this.pinned = timestamp;
        } else if (action === 'unpin') {
            this.pinned = null;
            this.hovered = null;
        } else if (action === 'hover' && Number.isFinite(timestamp)) {
            this.hovered = timestamp;
        } else if (action === 'clear') {
            this.hovered = null;
        }
        return {
            timestamp: this.pinned ?? this.hovered,
            pinned: this.pinned !== null,
        };
    }
}

export const sharedCrosshair = new CrosshairState();

function eventTimestamp(chart, event) {
    const scale = chart?.scales?.x;
    const area = chart?.chartArea;
    const rect = chart?.canvas?.getBoundingClientRect?.();
    if (!scale?.getValueForPixel || !area || !rect) return null;
    const x = event.clientX - rect.left;
    if (x < area.left || x > area.right) return null;
    const value = Number(scale.getValueForPixel(x));
    return Number.isFinite(value) ? value : null;
}

export function attachCrosshairEvents(chart) {
    const canvas = chart?.canvas;
    if (!canvas?.addEventListener) return () => {};

    const dragThresholdSquared = 16;
    let press = null;
    let suppressClick = false;
    let suppressClickTimer = null;

    const dispatch = (action, timestamp = null) => {
        document.dispatchEvent(new CustomEvent('kula-crosshair', {
            detail: { action, timestamp, chart },
        }));
    };
    const mouseMove = event => {
        if (press?.dragged) return;
        const timestamp = eventTimestamp(chart, event);
        if (timestamp !== null) dispatch('hover', timestamp);
    };
    const mouseLeave = () => dispatch('clear');
    const pointerDown = event => {
        if (event.button !== 0 || event.isPrimary === false) return;
        press = {
            id: event.pointerId,
            x: event.clientX,
            y: event.clientY,
            dragged: false,
        };
        dispatch('clear');
    };
    const pointerMove = event => {
        if (!press || event.pointerId !== press.id) return;
        const dx = event.clientX - press.x;
        const dy = event.clientY - press.y;
        if (dx * dx + dy * dy >= dragThresholdSquared) press.dragged = true;
    };
    const pointerEnd = event => {
        if (!press || event.pointerId !== press.id) return;
        if (press.dragged) {
            suppressClick = true;
            clearTimeout(suppressClickTimer);
            // The synthetic click following pointerup is dispatched first.
            // Expire the guard so a gesture that produces no click cannot
            // consume the user's next deliberate click.
            suppressClickTimer = setTimeout(() => { suppressClick = false; }, 250);
        }
        press = null;
    };
    const pointerCancel = event => {
        if (press && event.pointerId === press.id) press = null;
    };
    const wheel = event => {
        if (event.ctrlKey) dispatch('clear');
    };
    const click = event => {
        if (suppressClick) {
            suppressClick = false;
            clearTimeout(suppressClickTimer);
            suppressClickTimer = null;
            dispatch('clear');
            return;
        }
        const timestamp = eventTimestamp(chart, event);
        if (timestamp !== null) dispatch('pin', timestamp);
    };
    canvas.addEventListener('mousemove', mouseMove);
    canvas.addEventListener('mouseleave', mouseLeave);
    canvas.addEventListener('pointerdown', pointerDown);
    canvas.addEventListener('pointermove', pointerMove);
    canvas.addEventListener('pointerup', pointerEnd);
    canvas.addEventListener('pointercancel', pointerCancel);
    canvas.addEventListener('wheel', wheel, { passive: true });
    canvas.addEventListener('click', click);
    return () => {
        clearTimeout(suppressClickTimer);
        canvas.removeEventListener('mousemove', mouseMove);
        canvas.removeEventListener('mouseleave', mouseLeave);
        canvas.removeEventListener('pointerdown', pointerDown);
        canvas.removeEventListener('pointermove', pointerMove);
        canvas.removeEventListener('pointerup', pointerEnd);
        canvas.removeEventListener('pointercancel', pointerCancel);
        canvas.removeEventListener('wheel', wheel);
        canvas.removeEventListener('click', click);
    };
}

export const crosshairPlugin = {
    id: 'kulaCrosshair',
    afterDraw(chart) {
        const timestamp = chart.$kulaCrosshairTimestamp;
        const scale = chart?.scales?.x;
        const area = chart?.chartArea;
        if (!Number.isFinite(timestamp) || !scale || !area) return;
        const x = scale.getPixelForValue(timestamp);
        if (!Number.isFinite(x) || x < area.left || x > area.right) return;

        const ctx = chart.ctx;
        ctx.save();
        ctx.beginPath();
        ctx.moveTo(x, area.top);
        ctx.lineTo(x, area.bottom);
        ctx.lineWidth = 1;
        ctx.strokeStyle = 'rgba(148, 163, 184, 0.75)';
        ctx.setLineDash([4, 3]);
        ctx.stroke();
        ctx.restore();
    },
};

// Clear hover feedback before a gesture and keep it hidden until the next
// unpressed pointer move. Chart.js can otherwise redraw its last hover event
// while the zoom plugin is painting the selection rectangle.
export const tooltipGesturePlugin = {
    id: 'kulaTooltipGesture',
    beforeTooltipDraw(chart) {
        return !chart.$kulaTooltipSuppressed && !chart.isZoomingOrPanning?.();
    },
};

export function attachTooltipGestures(chart) {
    const canvas = chart.canvas;
    const hide = () => {
        chart.$kulaTooltipSuppressed = true;
        chart.tooltip?.setActiveElements([], { x: 0, y: 0 });
        chart.setActiveElements([]);
        chart.render();
    };
    const down = event => {
        if (event.button === 0 || event.pointerType === 'touch') hide();
    };
    const move = event => {
        if (!event.buttons && event.pointerType !== 'touch') chart.$kulaTooltipSuppressed = false;
    };
    const wheel = event => { if (event.ctrlKey) hide(); };
    canvas.addEventListener('pointerdown', down);
    canvas.addEventListener('pointermove', move);
    canvas.addEventListener('pointerleave', hide);
    canvas.addEventListener('wheel', wheel, { passive: true });
    return () => {
        canvas.removeEventListener('pointerdown', down);
        canvas.removeEventListener('pointermove', move);
        canvas.removeEventListener('pointerleave', hide);
        canvas.removeEventListener('wheel', wheel);
    };
}

function currentRange(chart) {
    const scale = chart?.scales?.x;
    const options = chart?.options?.scales?.x;
    const min = Number(options?.min ?? scale?.min);
    const max = Number(options?.max ?? scale?.max);
    return Number.isFinite(min) && Number.isFinite(max) && max > min ? { min, max } : null;
}

function plotGeometry(chart) {
    const area = chart?.chartArea;
    const canvas = chart?.canvas;
    const rect = canvas?.getBoundingClientRect?.();
    const left = area?.left ?? 0;
    const width = area?.width || canvas?.clientWidth || rect?.width || 1;
    return { left, width, rectLeft: rect?.left || 0 };
}

function applyRange(chart, range, complete) {
    if (!chart?.options?.scales?.x) return;
    chart.options.scales.x.min = range.min;
    chart.options.scales.x.max = range.max;
    document.dispatchEvent(new CustomEvent('kula-zoom-sync', {
        detail: { chart, complete },
    }));
}

export function attachChartGestures(chart) {
    const canvas = chart?.canvas;
    if (!canvas?.addEventListener) return () => {};

    canvas.tabIndex = canvas.tabIndex >= 0 ? canvas.tabIndex : 0;
    if (!canvas.getAttribute('aria-label') && !canvas.getAttribute('aria-labelledby')) {
        canvas.setAttribute('aria-label', 'Time-series chart. Use arrow keys to pan and plus or minus to zoom.');
    }
    canvas.style.touchAction = 'pan-y';

    const pointers = new Map();
    let lastSingle = null;
    let pinch = null;
    let changed = false;

    const point = event => ({ x: event.clientX, y: event.clientY, type: event.pointerType });
    const distance = values => Math.hypot(values[0].x - values[1].x, values[0].y - values[1].y);

    function pointerDown(event) {
        const isTouch = event.pointerType === 'touch';
        const isMousePan = event.pointerType === 'mouse' && event.shiftKey && event.button === 0;
        if (!isTouch && !isMousePan) return;

        pointers.set(event.pointerId, point(event));
        canvas.setPointerCapture?.(event.pointerId);
        if (pointers.size === 1) {
            lastSingle = point(event);
            pinch = null;
        } else if (pointers.size === 2) {
            const values = Array.from(pointers.values());
            const range = currentRange(chart);
            if (range) pinch = { distance: distance(values), range };
        }
        changed = false;
        event.preventDefault();
    }

    function pointerMove(event) {
        if (!pointers.has(event.pointerId)) return;
        pointers.set(event.pointerId, point(event));
        const range = currentRange(chart);
        if (!range) return;

        const geometry = plotGeometry(chart);
        if (pointers.size >= 2 && pinch) {
            const values = Array.from(pointers.values()).slice(0, 2);
            const nextDistance = distance(values);
            if (pinch.distance > 0 && nextDistance > 0) {
                const centerX = (values[0].x + values[1].x) / 2 - geometry.rectLeft;
                const anchor = (centerX - geometry.left) / geometry.width;
                applyRange(chart, zoomTimeRange(
                    pinch.range.min,
                    pinch.range.max,
                    pinch.distance / nextDistance,
                    anchor,
                ), false);
                changed = true;
            }
        } else if (lastSingle) {
            const next = point(event);
            const deltaX = next.x - lastSingle.x;
            if (deltaX !== 0) {
                applyRange(chart, panTimeRange(range.min, range.max, deltaX, geometry.width), false);
                changed = true;
            }
            lastSingle = next;
        }
        event.preventDefault();
    }

    function pointerEnd(event) {
        if (!pointers.has(event.pointerId)) return;
        pointers.delete(event.pointerId);
        canvas.releasePointerCapture?.(event.pointerId);
        if (pointers.size === 1) {
            lastSingle = Array.from(pointers.values())[0];
            pinch = null;
        } else if (pointers.size === 0) {
            lastSingle = null;
            pinch = null;
            if (changed) {
                const range = currentRange(chart);
                if (range) applyRange(chart, range, true);
            }
            changed = false;
        }
        event.preventDefault();
    }

    function keyDown(event) {
        const range = currentRange(chart);
        if (!range) return;

        const span = range.max - range.min;
        let next = null;
        if (event.key === 'ArrowLeft') next = { min: range.min - span * 0.1, max: range.max - span * 0.1 };
        if (event.key === 'ArrowRight') next = { min: range.min + span * 0.1, max: range.max + span * 0.1 };
        if (event.key === '+' || event.key === '=') next = zoomTimeRange(range.min, range.max, 0.8);
        if (event.key === '-' || event.key === '_') next = zoomTimeRange(range.min, range.max, 1.25);
        if (!next) return;

        event.preventDefault();
        applyRange(chart, next, true);
    }

    canvas.addEventListener('pointerdown', pointerDown);
    canvas.addEventListener('pointermove', pointerMove);
    canvas.addEventListener('pointerup', pointerEnd);
    canvas.addEventListener('pointercancel', pointerEnd);
    canvas.addEventListener('keydown', keyDown);

    return () => {
        canvas.removeEventListener('pointerdown', pointerDown);
        canvas.removeEventListener('pointermove', pointerMove);
        canvas.removeEventListener('pointerup', pointerEnd);
        canvas.removeEventListener('pointercancel', pointerEnd);
        canvas.removeEventListener('keydown', keyDown);
    };
}

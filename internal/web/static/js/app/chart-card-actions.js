/* ============================================================
   chart-card-actions.js — Shared interactions for static and
   dynamically created chart cards.
   ============================================================ */
'use strict';
import { state } from './state.js';

const ORDER_STEP = 10;
const ROW_TOLERANCE_PX = 10;

function chartForCanvas(canvas) {
    const chartClass = globalThis.Chart;
    if (!canvas || typeof chartClass?.getChart !== 'function') return null;
    return chartClass.getChart(canvas) || null;
}

function syncPauseState() {
    document.dispatchEvent(new Event('kula-sync-pause'));
}

function renderedCards(grid) {
    return Array.from(grid.querySelectorAll('.chart-card:not(.hidden)')).filter(card => {
        const style = window.getComputedStyle(card);
        return style.display !== 'none' && style.visibility !== 'hidden';
    });
}

function snapshotOriginalOrders(cards) {
    cards.forEach(card => {
        if (card.dataset.expandOrigOrder === undefined) {
            card.dataset.expandOrigOrder = card.style.order;
        }
    });
}

function cardsInVisualOrder(grid, cards) {
    const rtl = window.getComputedStyle(grid).direction === 'rtl';
    return cards.map((card, index) => ({
        card,
        index,
        rect: card.getBoundingClientRect(),
    })).sort((a, b) => {
        const rowDelta = a.rect.top - b.rect.top;
        if (Math.abs(rowDelta) >= ROW_TOLERANCE_PX) return rowDelta;

        const columnDelta = rtl ? b.rect.left - a.rect.left : a.rect.left - b.rect.left;
        return columnDelta || a.index - b.index;
    }).map(entry => entry.card);
}

// Give every rendered card a temporary, evenly-spaced order while one or
// more cards are expanded. The spacing leaves room to move a card to the
// front of its current row without changing the row's visual position.
function normalizeVisualOrders(grid, cards) {
    const ordered = cardsInVisualOrder(grid, cards);
    ordered.forEach((card, index) => {
        card.style.order = String((index + 1) * ORDER_STEP);
    });
    return ordered;
}

function restoreOriginalOrders(grid) {
    grid.querySelectorAll('.chart-card[data-expand-orig-order]').forEach(card => {
        card.style.order = card.dataset.expandOrigOrder;
        delete card.dataset.expandOrigOrder;
    });
}

function resizeCardChart(card) {
    const chart = chartForCanvas(card.querySelector('canvas'));
    if (chart) setTimeout(() => chart.resize(), 50);
}

// ---- Hover Pause ----
// Wire hover/touch pause on a single chart card (idempotent).
export function attachHoverPauseToCard(card) {
    if (!card || card.dataset.hoverPause === '1') return;
    card.dataset.hoverPause = '1';

    card.addEventListener('mouseenter', () => {
        if (!state.pausedHover) {
            state.pausedHover = true;
            syncPauseState();
        }
    });
    card.addEventListener('mouseleave', () => {
        if (state.pausedHover) {
            state.pausedHover = false;
            syncPauseState();
        }
    });

    // Touch events for mobile
    card.addEventListener('touchstart', () => {
        if (!state.pausedHover) {
            state.pausedHover = true;
            syncPauseState();
        }
    }, { passive: true });

    const resumeFromTouch = () => {
        if (state.pausedHover) {
            state.pausedHover = false;
            syncPauseState();
        }

        const chart = chartForCanvas(card.querySelector('canvas'));
        if (chart?.tooltip) {
            chart.tooltip.setActiveElements([], { x: 0, y: 0 });
            chart.update();
        }
    };

    card.addEventListener('touchend', resumeFromTouch, { passive: true });
    card.addEventListener('touchcancel', resumeFromTouch, { passive: true });
}

// ---- Chart Expand / Collapse ----
export function toggleExpandChart(cardId) {
    const card = document.getElementById(cardId);
    if (!card) return;

    const grid = card.closest('.charts-grid');
    if (!grid) return;

    const isExpanding = !card.classList.contains('chart-expanded');
    let cards = renderedCards(grid);

    if (isExpanding) {
        snapshotOriginalOrders(cards);
        cards = normalizeVisualOrders(grid, cards);

        // Move the card immediately before the first card currently occupying
        // its row. Temporary orders are spaced by ORDER_STEP, so the midpoint
        // remains between the previous row and this one.
        const myTop = card.getBoundingClientRect().top;
        const sameRowCards = cards.filter(candidate =>
            Math.abs(candidate.getBoundingClientRect().top - myTop) < ROW_TOLERANCE_PX
        );
        if (sameRowCards.length > 0) {
            const firstInRow = sameRowCards[0];
            const firstOrder = parseInt(firstInRow.style.order, 10);
            card.style.order = String(firstOrder - (ORDER_STEP / 2));
        }
    }

    const isExpanded = card.classList.toggle('chart-expanded');

    if (!isExpanded) {
        const remainingExpanded = renderedCards(grid)
            .some(candidate => candidate.classList.contains('chart-expanded'));
        if (remainingExpanded) {
            // Keep all temporary orders in one consistent scale until the last
            // expanded card collapses.
            normalizeVisualOrders(grid, renderedCards(grid));
        } else {
            restoreOriginalOrders(grid);
        }
    }

    const btn = card.querySelector('.btn-expand-chart');
    if (btn) {
        btn.title = isExpanded ? 'Collapse chart' : 'Expand chart';
        btn.setAttribute('aria-label', btn.title);
        btn.setAttribute('aria-expanded', String(isExpanded));
    }

    resizeCardChart(card);
}

// Add the expand button to a chart card header (idempotent).
export function addExpandButton(card) {
    if (!card?.id || card.querySelector('.btn-expand-chart')) return;

    const header = card.querySelector('.chart-header');
    if (!header) return;

    let actions = header.querySelector('.chart-header-right');
    if (!actions) {
        actions = document.createElement('div');
        actions.className = 'chart-header-right';
        header.appendChild(actions);
    }

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn-icon btn-expand-chart';
    btn.title = 'Expand chart';
    btn.setAttribute('aria-label', btn.title);
    btn.setAttribute('aria-expanded', 'false');
    btn.textContent = '🔍';
    btn.addEventListener('click', event => {
        event.stopPropagation();
        toggleExpandChart(card.id);
    });
    actions.appendChild(btn);
}

// Attach all interactions needed by a card created after page initialization.
export function attachDynamicChartCardActions(card, resetZoom) {
    addExpandButton(card);
    attachHoverPauseToCard(card);

    const canvas = card?.querySelector('canvas');
    if (canvas && resetZoom && canvas.dataset.zoomReset !== '1') {
        canvas.dataset.zoomReset = '1';
        canvas.addEventListener('dblclick', resetZoom);
    }
}

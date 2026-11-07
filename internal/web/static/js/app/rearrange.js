/* ============================================================
   rearrange.js — Rearrange mode: move gauges and chart cards
   within their section by mouse, pen, touch (from a card's grip)
   or keyboard. card-layout.js stores the order and applies it
   everywhere, Focus and TV mode included.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { i18n } from './i18n.js';
import { CARD_SECTIONS, dropIndex } from './card-order.js';
import {
    arrangeCards,
    getStoredCardOrder,
    registerCardDecorator,
    saveSectionOrder,
    sectionCards,
    sectionOf,
    setDragSection,
    setStoredCardOrder,
} from './card-layout.js';
import { collapseExpandedCards } from './chart-card-actions.js';
import { closeSettingsMenu } from './settings.js';

const DRAG_THRESHOLD_PX = 4;
const EDGE_SCROLL_PX = 48;
const MAX_SCROLL_STEP_PX = 18;
const DROP_MARGIN_PX = 32;
const CLICK_SUPPRESS_MS = 250;

let initialized = false;
// The stored order when the mode was entered, restored by Cancel.
let snapshot = null;
let bar = null;
let liveRegion = null;
let drag = null;
let suppressClickUntil = 0;

function translated(element, key) {
    element.dataset.i18n = key;
    element.textContent = i18n.t(key);
}

function cardName(card) {
    const label = card.querySelector('.chart-header h3, .gauge-label');
    return (label?.textContent || card.id).trim();
}

function isRendered(card) {
    return card.getClientRects().length > 0;
}

function renderedSection(section) {
    return sectionCards(section).filter(isRendered);
}

function isRtl() {
    return document.documentElement.dir === 'rtl';
}

function announce(text) {
    if (liveRegion) liveRegion.textContent = text;
}

function announceMove(card) {
    const cards = renderedSection(sectionOf(card));
    announce(i18n.t('card_moved')
        .replace('{name}', cardName(card))
        .replace('{position}', String(cards.indexOf(card) + 1))
        .replace('{total}', String(cards.length)));
}

// ---- Grips ----

function gripIcon() {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('viewBox', '0 0 10 16');
    svg.setAttribute('width', '10');
    svg.setAttribute('height', '16');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    for (const cx of [3, 7]) {
        for (const cy of [3, 8, 13]) {
            const dot = document.createElementNS(ns, 'circle');
            dot.setAttribute('cx', String(cx));
            dot.setAttribute('cy', String(cy));
            dot.setAttribute('r', '1.5');
            dot.setAttribute('fill', 'currentColor');
            svg.appendChild(dot);
        }
    }
    return svg;
}

function labelGrip(grip, card) {
    const label = i18n.t('move_card').replace('{name}', cardName(card));
    grip.setAttribute('aria-label', label);
    grip.title = label;
}

// Give a card its grip and take everything else in it out of reach: inert
// stops chart gestures, tooltips, header buttons and their Tab stops.
function decorate(card) {
    if (!state.rearranging || !card?.dataset.cardSection || card.querySelector(':scope > .card-grip')) return;
    const grip = document.createElement('button');
    grip.type = 'button';
    grip.className = 'card-grip';
    grip.setAttribute('aria-describedby', 'rearrange-hint');
    grip.appendChild(gripIcon());
    labelGrip(grip, card);
    grip.addEventListener('keydown', event => onGripKey(event, card));
    for (const child of card.children) {
        if (child.inert) continue;
        child.inert = true;
        child.dataset.rearrangeInert = '';
    }
    card.prepend(grip);
}

function undecorate(card) {
    card.classList.remove('card-shifting', 'card-dragging');
    card.style.transform = '';
    card.querySelector(':scope > .card-grip')?.remove();
    card.querySelectorAll(':scope > [data-rearrange-inert]').forEach(child => {
        child.inert = false;
        delete child.dataset.rearrangeInert;
    });
}

function allCards() {
    return CARD_SECTIONS.flatMap(section => sectionCards(section));
}

// ---- Keyboard ----

function moveAmong(card, others, index) {
    if (index < others.length) others[index].before(card);
    else if (others.length > 0) others[others.length - 1].after(card);
}

function onGripKey(event, card) {
    if (drag) return;
    const rtl = isRtl();
    const steps = {
        ArrowUp: 'previous',
        ArrowDown: 'next',
        ArrowLeft: rtl ? 'next' : 'previous',
        ArrowRight: rtl ? 'previous' : 'next',
        Home: 'first',
        End: 'last',
    };
    const step = steps[event.key];
    if (!step) return;
    event.preventDefault();

    const section = sectionOf(card);
    const cards = renderedSection(section);
    const index = cards.indexOf(card);
    if (index < 0) return;
    const others = cards.filter(other => other !== card);
    const target = {
        previous: Math.max(0, index - 1),
        next: Math.min(others.length, index + 1),
        first: 0,
        last: others.length,
    }[step];
    if (target !== index) {
        moveAmong(card, others, target);
        saveSectionOrder(section);
    }
    // Moving a card takes focus from its grip.
    card.querySelector(':scope > .card-grip')?.focus({ preventScroll: true });
    card.scrollIntoView({ block: 'nearest' });
    announceMove(card);
}

// ---- Pointer ----

function topEdge() {
    const topbar = document.querySelector('.sticky-topbar');
    if (!topbar || getComputedStyle(topbar).position !== 'sticky') return 0;
    return Math.max(0, topbar.getBoundingClientRect().bottom);
}

function scrollStep(y) {
    const top = topEdge();
    const bottom = window.innerHeight;
    if (y < top + EDGE_SCROLL_PX) {
        return -Math.ceil(MAX_SCROLL_STEP_PX * Math.min(1, (top + EDGE_SCROLL_PX - y) / EDGE_SCROLL_PX));
    }
    if (y > bottom - EDGE_SCROLL_PX) {
        return Math.ceil(MAX_SCROLL_STEP_PX * Math.min(1, (y - bottom + EDGE_SCROLL_PX) / EDGE_SCROLL_PX));
    }
    return 0;
}

function withinDropArea(container, x, y) {
    const rect = container.getBoundingClientRect();
    return x >= rect.left - DROP_MARGIN_PX && x <= rect.right + DROP_MARGIN_PX
        && y >= rect.top - DROP_MARGIN_PX && y <= rect.bottom + DROP_MARGIN_PX;
}

// Slide the cards a move displaced from their old place to the new one.
function slide(cards, before) {
    cards.forEach((card, index) => {
        const after = card.getBoundingClientRect();
        const dx = before[index].left - after.left;
        const dy = before[index].top - after.top;
        if (!dx && !dy) return;
        card.classList.remove('card-shifting');
        card.style.transform = `translate(${dx}px, ${dy}px)`;
        card.getBoundingClientRect();
        card.classList.add('card-shifting');
        card.style.transform = '';
    });
}

// Keep the lifted card under the pointer wherever its slot is now.
function follow() {
    const { card } = drag;
    const rect = card.getBoundingClientRect();
    const slotLeft = rect.left - drag.offsetX;
    const slotTop = rect.top - drag.offsetY;
    drag.offsetX = drag.x - drag.grabX - slotLeft;
    drag.offsetY = drag.y - drag.grabY - slotTop;
    card.style.transform = `translate(${drag.offsetX}px, ${drag.offsetY}px)`;
}

function reposition() {
    const { card, container } = drag;
    if (!withinDropArea(container, drag.x, drag.y)) return;
    const others = renderedSection(drag.section).filter(other => other !== card);
    const rects = others.map(other => other.getBoundingClientRect());
    const target = dropIndex(rects, drag.x, drag.y, isRtl());
    const current = renderedSection(drag.section).indexOf(card);
    if (target === current) return;
    moveAmong(card, others, target);
    slide(others, rects);
}

function frame() {
    drag.frame = 0;
    if (!drag.card.isConnected) {
        // A split rebuild replaced the card being dragged.
        endDrag(false);
        return;
    }
    const step = scrollStep(drag.y);
    if (step) window.scrollBy(0, step);
    reposition();
    follow();
    if (step) requestFrame();
}

function requestFrame() {
    if (drag && !drag.frame) drag.frame = requestAnimationFrame(frame);
}

function startDrag() {
    const { card, container } = drag;
    drag.active = true;
    try {
        // Capture on the container: moving the captured card would release it.
        container.setPointerCapture(drag.pointerId);
    } catch (_error) { /* the pointer is already gone */ }
    const rect = card.getBoundingClientRect();
    drag.grabX = drag.startX - rect.left;
    drag.grabY = drag.startY - rect.top;
    drag.offsetX = 0;
    drag.offsetY = 0;
    setDragSection(drag.section);
    card.classList.remove('card-shifting');
    card.classList.add('card-dragging');
    document.getElementById('dashboard')?.classList.add('card-drag-active');
    window.addEventListener('keydown', onDragKey, true);
}

function onPointerDown(event) {
    if (!state.rearranging || drag || !event.isPrimary) return;
    if (event.pointerType === 'mouse' && event.button !== 0) return;
    const card = event.target.closest?.('[data-card-section]');
    if (!card || !card.parentElement) return;
    // Touch scrolls the page unless it starts on the grip.
    if (event.pointerType === 'touch' && !event.target.closest('.card-grip')) return;
    drag = {
        card,
        section: sectionOf(card),
        container: card.parentElement,
        pointerId: event.pointerId,
        startX: event.clientX,
        startY: event.clientY,
        x: event.clientX,
        y: event.clientY,
        active: false,
        frame: 0,
    };
    window.addEventListener('pointermove', onPointerMove);
    window.addEventListener('pointerup', onPointerUp);
    window.addEventListener('pointercancel', onPointerUp);
}

function onPointerMove(event) {
    if (!drag || event.pointerId !== drag.pointerId) return;
    drag.x = event.clientX;
    drag.y = event.clientY;
    if (!drag.active) {
        if (Math.hypot(drag.x - drag.startX, drag.y - drag.startY) < DRAG_THRESHOLD_PX) return;
        startDrag();
    }
    event.preventDefault();
    requestFrame();
}

// pointercancel drops the card where it is, like pointerup.
function onPointerUp(event) {
    if (!drag || event.pointerId !== drag.pointerId) return;
    if (drag.active) endDrag(true);
    else endDrag(false);
}

function onDragKey(event) {
    if (event.key !== 'Escape' || !drag?.active) return;
    // Keep TV mode, the settings menu and the AI panel from acting on it too.
    event.preventDefault();
    event.stopImmediatePropagation();
    endDrag(false);
}

function endDrag(keep) {
    const current = drag;
    drag = null;
    window.removeEventListener('pointermove', onPointerMove);
    window.removeEventListener('pointerup', onPointerUp);
    window.removeEventListener('pointercancel', onPointerUp);
    if (!current.active) return;

    if (current.frame) cancelAnimationFrame(current.frame);
    window.removeEventListener('keydown', onDragKey, true);
    try {
        current.container.releasePointerCapture(current.pointerId);
    } catch (_error) { /* already released */ }
    setDragSection(null);
    document.getElementById('dashboard')?.classList.remove('card-drag-active');
    suppressClickUntil = performance.now() + CLICK_SUPPRESS_MS;

    const { card } = current;
    card.classList.remove('card-dragging');
    // Settle into the slot from wherever the pointer left it.
    card.classList.add('card-shifting');
    card.style.transform = '';
    if (keep && card.isConnected) {
        saveSectionOrder(current.section);
        announceMove(card);
    } else {
        // Nothing was saved: the stored order puts the cards back.
        arrangeCards();
    }
}

// ---- Mode ----

function buildBar() {
    bar = document.createElement('div');
    bar.id = 'rearrange-bar';
    bar.className = 'focus-bar rearrange-bar';
    bar.setAttribute('role', 'region');
    bar.dataset.i18nAriaLabel = 'rearrange_cards';
    bar.setAttribute('aria-label', i18n.t('rearrange_cards'));
    bar.tabIndex = -1;

    const hint = document.createElement('span');
    hint.id = 'rearrange-hint';
    translated(hint, 'rearrange_hint');

    liveRegion = document.createElement('span');
    liveRegion.className = 'sr-only';
    liveRegion.setAttribute('role', 'status');
    liveRegion.setAttribute('aria-live', 'polite');

    const buttons = [
        ['btn-rearrange-reset', 'reset_order', () => {
            setStoredCardOrder(null);
            announce(i18n.t('card_order_reset'));
        }],
        ['btn-rearrange-cancel', 'cancel', () => exitRearrange(true)],
        ['btn-rearrange-done', 'done', () => exitRearrange(false)],
    ].map(([id, key, onClick]) => {
        const button = document.createElement('button');
        button.type = 'button';
        button.id = id;
        translated(button, key);
        button.addEventListener('click', onClick);
        return button;
    });

    bar.append(hint, liveRegion, ...buttons);
    const topbar = document.querySelector('.sticky-topbar');
    if (topbar) topbar.appendChild(bar);
    else document.getElementById('dashboard-home')?.prepend(bar);
}

export function enterRearrange() {
    if (state.rearranging || state.focusSelecting) return;
    closeSettingsMenu();
    // Expanded cards sit out of order; a search hides the cards around them.
    collapseExpandedCards(document);
    const search = document.getElementById('chart-search');
    if (search?.value) {
        search.value = '';
        search.dispatchEvent(new Event('input'));
    }
    snapshot = getStoredCardOrder();
    state.rearranging = true;
    document.getElementById('dashboard')?.classList.add('rearranging');
    const focusButton = document.getElementById('btn-focus');
    if (focusButton) focusButton.disabled = true;
    buildBar();
    allCards().forEach(decorate);
    bar.focus({ preventScroll: true });
}

/** Leave Rearrange mode; restore puts back the order it was entered with. */
export function exitRearrange(restore = false) {
    if (!state.rearranging) return;
    if (drag) endDrag(false);
    if (restore) setStoredCardOrder(snapshot);
    snapshot = null;
    state.rearranging = false;
    allCards().forEach(undecorate);
    bar?.remove();
    bar = null;
    liveRegion = null;
    document.getElementById('dashboard')?.classList.remove('rearranging');
    const focusButton = document.getElementById('btn-focus');
    if (focusButton) focusButton.disabled = false;
    document.getElementById('btn-settings')?.focus({ preventScroll: true });
}

export function initRearrange() {
    if (initialized) return;
    initialized = true;
    registerCardDecorator(decorate);

    const button = document.getElementById('btn-rearrange');
    button?.addEventListener('click', enterRearrange);
    // Focus mode's selection and Rearrange mode both take over the cards.
    document.getElementById('btn-settings')?.addEventListener('click', () => {
        if (button) button.disabled = state.focusSelecting || state.rearranging;
    });

    document.getElementById('dashboard-home')?.addEventListener('pointerdown', onPointerDown);
    // The click that ends a drag is not a click on the card under it.
    window.addEventListener('click', event => {
        if (performance.now() >= suppressClickUntil) return;
        suppressClickUntil = 0;
        event.preventDefault();
        event.stopPropagation();
    }, true);
    document.addEventListener('kula-i18n-changed', () => {
        if (!state.rearranging) return;
        allCards().forEach(card => {
            const grip = card.querySelector(':scope > .card-grip');
            if (grip) labelGrip(grip, card);
        });
    });
}

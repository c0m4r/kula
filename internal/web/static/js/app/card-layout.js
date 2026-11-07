/* ============================================================
   card-layout.js — The one place that decides where dashboard
   cards sit. Template cards are registered at startup; every
   card created later (split, battery/UPS, application) enters
   through placeCard(). arrangeCards() then puts every card in
   its section's order — the default order, or the visitor's
   rearrangement — so the layout depends only on which cards
   exist, the stored order and whether Focus mode combines the
   sections, never on the order events happened in.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { readJsonPref, removePref, writeJsonPref } from './prefs.js';
import { updateChartUI } from './chart-ui.js';
import {
    CARD_FAMILY_ORDER,
    defaultCardOrder,
    mergeSavedOrder,
    resolveCardOrder,
    sanitizeCardOrder,
} from './card-order.js';
import { adoptExpandOrder, collapseExpandedCards, settleExpandOrders } from './chart-card-actions.js';

const STORAGE_KEY = 'kula_card_order';

// Each section's home container. Focus mode shows the chart sections combined
// in #charts-grid, in this order.
const SECTIONS = [
    { key: 'gauges', container: 'gauges-row', cards: '.gauge-card' },
    { key: 'system', container: 'charts-grid', cards: '.chart-card' },
    { key: 'thermals', container: 'thermals-grid', cards: '.chart-card' },
    { key: 'applications', container: 'applications-grid', cards: '.chart-card' },
];
const COMBINED_CONTAINER = 'charts-grid';

let stored = sanitizeCardOrder(readJsonPref(STORAGE_KEY, null)) || {};
let initialized = false;
let lastCombined = null;
let dragSection = null;
const decorators = [];

function sectionDef(key) {
    return SECTIONS.find(section => section.key === key) || null;
}

function stamp(card, { section, family = 'static', anchor = '', rank = 0 }) {
    card.dataset.cardSection = section;
    card.dataset.cardFamily = family;
    if (anchor) card.dataset.cardAnchor = anchor;
    else delete card.dataset.cardAnchor;
    card.dataset.cardRank = String(Number.isFinite(rank) ? rank : 0);
}

/** Register the template's cards in their template order (idempotent). */
export function initCardLayout() {
    if (initialized) return;
    initialized = true;
    for (const section of SECTIONS) {
        const container = document.getElementById(section.container);
        if (!container) continue;
        let rank = 0;
        for (const card of container.querySelectorAll(`:scope > ${section.cards}[id]`)) {
            stamp(card, { section: section.key, rank: rank++ });
        }
    }
}

function combinedMode() {
    return !!(state.focusMode && !state.focusSelecting);
}

function targetContainer(sectionKey, combined) {
    const section = sectionDef(sectionKey);
    if (!section) return null;
    const id = combined && sectionKey !== 'gauges' ? COMBINED_CONTAINER : section.container;
    return document.getElementById(id);
}

/** The cards of one section, in document order. */
export function sectionCards(sectionKey) {
    return Array.from(document.querySelectorAll(`[data-card-section="${sectionKey}"]`));
}

/** Every chart card the layout manages (gauges excluded), in document order. */
export function chartCards() {
    return Array.from(document.querySelectorAll('.chart-card[data-card-section]'));
}

export function sectionOf(card) {
    return card?.dataset?.cardSection || null;
}

function describe(card) {
    return {
        id: card.id,
        family: card.dataset.cardFamily || 'static',
        anchor: card.dataset.cardAnchor || '',
        rank: Number(card.dataset.cardRank),
    };
}

function resolvedSection(sectionKey) {
    const cards = sectionCards(sectionKey).filter(card => card.id);
    const byId = new Map(cards.map(card => [card.id, card]));
    const meta = {};
    const described = cards.map(card => {
        const info = describe(card);
        meta[info.id] = info;
        return info;
    });
    const order = defaultCardOrder(described, CARD_FAMILY_ORDER);
    // While a card is being dragged its section follows the working order, so
    // a card created mid-drag does not snap the drag back.
    const saved = sectionKey === dragSection
        ? mergeSavedOrder(cards.map(card => card.id), stored[sectionKey])
        : stored[sectionKey];
    return resolveCardOrder(order, saved, meta).map(id => byId.get(id));
}

// Move only the cards that are out of place; an arranged container is left
// untouched. Every move is synchronous, so no chart is ever left detached.
function placeInOrder(container, cards) {
    let cursor = container.firstElementChild;
    const moved = [];
    for (const card of cards) {
        if (card === cursor) {
            cursor = cursor.nextElementSibling;
            continue;
        }
        container.insertBefore(card, cursor);
        moved.push(card);
    }
    settleExpandOrders(container);
    moved.forEach(adoptExpandOrder);
}

/** Put every card where its section's order says, for the current mode. */
export function arrangeCards() {
    initCardLayout();
    const combined = combinedMode();
    // Expanded cards carry temporary CSS orders that belong to the grid they
    // were expanded in.
    if (lastCombined !== null && combined !== lastCombined) collapseExpandedCards(document);
    lastCombined = combined;

    const plan = new Map();
    for (const section of SECTIONS) {
        const container = targetContainer(section.key, combined);
        if (!container) continue;
        if (!plan.has(container)) plan.set(container, []);
        plan.get(container).push(...resolvedSection(section.key));
    }
    for (const [container, cards] of plan) placeInOrder(container, cards);
}

/**
 * Add a card created after startup. slot: { section, family, anchor, rank }.
 * The card is in its container before this returns, so its chart can be
 * created and measured; its position is settled with the batch it arrived in.
 */
export function placeCard(card, slot) {
    initCardLayout();
    stamp(card, slot);
    const container = targetContainer(slot.section, combinedMode());
    if (container) container.appendChild(card);
    for (const decorate of decorators) decorate(card);
    updateChartUI('card-layout', arrangeCards);
}

/** Run fn(card) for every card placed from now on (focus classes, grips). */
export function registerCardDecorator(fn) {
    decorators.push(fn);
}

/** Resolve sectionKey against its current DOM order while a drag is running. */
export function setDragSection(sectionKey) {
    dragSection = sectionKey;
}

/** Store a section's current DOM order, keeping the ids of absent cards. */
export function saveSectionOrder(sectionKey) {
    if (!sectionDef(sectionKey)) return;
    const ids = sectionCards(sectionKey).map(card => card.id).filter(Boolean);
    stored = { ...stored, [sectionKey]: mergeSavedOrder(ids, stored[sectionKey]) };
    writeJsonPref(STORAGE_KEY, stored);
}

/** A copy of the stored order, for restoring it later. */
export function getStoredCardOrder() {
    return JSON.parse(JSON.stringify(stored));
}

/** Replace the stored order (null or {} restores the default) and apply it. */
export function setStoredCardOrder(order) {
    stored = sanitizeCardOrder(order) || {};
    if (Object.keys(stored).length === 0) removePref(STORAGE_KEY);
    else writeJsonPref(STORAGE_KEY, stored);
    arrangeCards();
}

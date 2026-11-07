/* ============================================================
   card-order.js — Ordering rules for dashboard cards.
   Pure functions, no imports: card-layout.js feeds them the
   cards it finds and applies the result, and the Node tests
   load this module directly.

   A card is { id, family, anchor, rank }. family is 'static'
   for template cards, 'split:<type>', 'psu' or 'app:<group>';
   anchor names the template card a split card belongs to; rank
   orders cards inside their family. Ties break on the id, never
   on arrival order, so the same cards always sort the same way.
   ============================================================ */
'use strict';

export const CARD_SECTIONS = ['gauges', 'system', 'thermals', 'applications'];

// Default order of the application groups. Each group is its own family, so a
// group's cards stay together whatever ranks its neighbours use.
export const APP_GROUPS = ['nginx', 'apache2', 'containers', 'postgres', 'mysql', 'custom'];

// Families without an anchor card, in their default order after the template
// cards: batteries/UPSes in System Metrics, then the application groups.
export const CARD_FAMILY_ORDER = ['psu', ...APP_GROUPS.map(group => `app:${group}`)];

const MAX_SECTION_IDS = 300;
const MAX_ID_LENGTH = 200;

function compareIds(a, b) {
    if (a.id < b.id) return -1;
    return a.id > b.id ? 1 : 0;
}

function compareRanks(a, b) {
    return (a.rank - b.rank) || compareIds(a, b);
}

function rankOf(card) {
    return Number.isFinite(card.rank) ? card.rank : 0;
}

/**
 * The order a section shows before anyone rearranges it: template cards in
 * template order, each followed by the split cards anchored to it, then the
 * families without an anchor in familyOrder (unknown families last, by name).
 */
export function defaultCardOrder(cards, familyOrder = []) {
    const list = cards.map(card => ({ ...card, rank: rankOf(card) }));
    const statics = list.filter(card => card.family === 'static').sort(compareRanks);
    const staticIds = new Set(statics.map(card => card.id));
    const chains = new Map();
    const loose = [];
    for (const card of list) {
        if (card.family === 'static') continue;
        if (card.anchor && staticIds.has(card.anchor)) {
            if (!chains.has(card.anchor)) chains.set(card.anchor, []);
            chains.get(card.anchor).push(card);
        } else {
            loose.push(card);
        }
    }

    const familyIndex = family => {
        const index = familyOrder.indexOf(family);
        return index < 0 ? familyOrder.length : index;
    };
    loose.sort((a, b) => (familyIndex(a.family) - familyIndex(b.family))
        || (a.family < b.family ? -1 : a.family > b.family ? 1 : 0)
        || compareRanks(a, b));

    const order = [];
    for (const card of statics) {
        order.push(card.id);
        for (const child of (chains.get(card.id) || []).sort(compareRanks)) order.push(child.id);
    }
    for (const card of loose) order.push(card.id);
    return order;
}

/**
 * Apply a stored order to the cards present now. Stored cards keep their
 * stored order. Each card the stored order does not know goes:
 *   1. after the nearest card that precedes it by default and is of its family
 *      or is its anchor (past cards that also sit between the two by default),
 *   2. else before the nearest card of its family that follows it by default,
 *   3. else at the end of the section.
 * So a new battery joins the batteries, a new interface follows its siblings or
 * its parent card, and a card of a family never arranged joins the end.
 *
 * order: ids in default order; stored: saved ids; meta: id -> { family, anchor }.
 */
export function resolveCardOrder(order, stored, meta = {}) {
    const position = new Map(order.map((id, index) => [id, index]));
    const out = [...new Set(Array.isArray(stored) ? stored : [])].filter(id => position.has(id));
    if (out.length === 0) return order.slice();

    const placed = new Set(out);
    const familyOf = id => meta[id]?.family || 'static';
    order.forEach((id, index) => {
        if (placed.has(id)) return;
        const family = familyOf(id);
        const anchor = meta[id]?.anchor;
        let at = -1;
        for (let before = index - 1; before >= 0 && at < 0; before--) {
            const candidate = order[before];
            if (!placed.has(candidate) || (familyOf(candidate) !== family && candidate !== anchor)) continue;
            at = out.indexOf(candidate) + 1;
            while (at < out.length && position.get(out[at]) > before && position.get(out[at]) < index) at++;
        }
        for (let after = index + 1; after < order.length && at < 0; after++) {
            const candidate = order[after];
            if (placed.has(candidate) && familyOf(candidate) === family) at = out.indexOf(candidate);
        }
        out.splice(at < 0 ? out.length : at, 0, id);
        placed.add(id);
    });
    return out;
}

/**
 * The order to store after a rearrangement: the section's cards as they sit
 * now, plus the stored ids that are absent at the moment (an application that
 * is down, a split view that is off), each kept after its old predecessor.
 * Past the cap, absent ids go first, from the end.
 */
export function mergeSavedOrder(current, previous, cap = MAX_SECTION_IDS) {
    const out = [...new Set(current)];
    const present = new Set(out);
    let after = null;
    for (const id of Array.isArray(previous) ? previous : []) {
        if (!present.has(id) && !out.includes(id)) {
            out.splice(after === null ? 0 : out.indexOf(after) + 1, 0, id);
        }
        after = id;
    }
    for (let index = out.length - 1; index >= 0 && out.length > cap; index--) {
        if (!present.has(out[index])) out.splice(index, 1);
    }
    return out;
}

/**
 * Accept only { section: [unique card ids] } for the known sections. Ids are
 * compared and looked up, never used as selectors or markup.
 */
export function sanitizeCardOrder(raw) {
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
    const clean = {};
    for (const section of CARD_SECTIONS) {
        if (!Object.prototype.hasOwnProperty.call(raw, section)) continue;
        const ids = raw[section];
        if (!Array.isArray(ids)) continue;
        const kept = new Set();
        for (const id of ids) {
            if (kept.size >= MAX_SECTION_IDS) break;
            if (typeof id === 'string' && id !== '' && id.length <= MAX_ID_LENGTH) kept.add(id);
        }
        if (kept.size > 0) clean[section] = [...kept];
    }
    return clean;
}

/**
 * Where a dragged card drops among its siblings' rects (the dragged card
 * itself excluded): the number of siblings before the pointer. A sibling is
 * before it when its row lies wholly above the pointer, or when it shares the
 * pointer's row and its centre is on the leading side.
 */
export function dropIndex(rects, x, y, rtl = false) {
    let index = 0;
    for (const rect of rects) {
        if (rect.bottom <= y) {
            index++;
        } else if (rect.top <= y) {
            const centre = rect.left + (rect.right - rect.left) / 2;
            if (rtl ? centre > x : centre < x) index++;
        }
    }
    return index;
}

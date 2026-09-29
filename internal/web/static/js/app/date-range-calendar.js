// Calendar days use UTC only as a timezone-neutral representation. The caller
// converts the chosen day strings to instants in the selected display zone.
const DAY = 86400000;
const key = day => new Date(day).toISOString().slice(0, 10);
const readDay = value => /^\d{4}-\d{2}-\d{2}$/.test(value || '') ? Date.parse(`${value}T00:00:00Z`) : NaN;
const dayOrNull = value => Number.isFinite(readDay(value)) ? readDay(value) : null;
const monthOf = day => {
    const date = new Date(day);
    return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1);
};
// Moves by whole months, keeping the day number where the target month has it.
const shiftMonths = (day, offset) => {
    const date = new Date(day);
    const first = Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + offset, 1);
    const length = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + offset + 1, 0)).getUTCDate();
    return first + (Math.min(date.getUTCDate(), length) - 1) * DAY;
};

// A single-month range calendar. The From and To fields own the draft; the
// calendar reads it through getRange and reports day changes through onSelect.
// `endpoint` is the end the next click sets, mirrored by the highlighted field.
export function attachRangeCalendar(root, {
    getRange,
    onSelect,
    onEndpointChange = () => {},
    translate,
    locale,
    today,
    isDayAvailable = () => true,
    dataBounds = () => null,
    maxDays = 31,
}) {
    let month;
    let start = null;
    let end = null;
    let endpoint = 'start';
    let focusedDay;

    const element = (tag, className, text) => {
        const node = document.createElement(tag);
        if (className) node.className = className;
        if (text !== undefined) node.textContent = text;
        return node;
    };
    const button = (className, text, label, action) => {
        const node = element('button', className, text);
        node.type = 'button';
        node.setAttribute('aria-label', label);
        node.addEventListener('click', action);
        return node;
    };
    const unavailable = cell => cell.getAttribute('aria-disabled') === 'true';

    // The range a click on day produces. Setting the start keeps an end that
    // still fits, so either end can be adjusted alone; an end before the start
    // moves the start, and one beyond the maximum span starts a new range.
    function choose(day) {
        const fits = last => last !== null && last >= day && last - day < maxDays * DAY;
        if (endpoint === 'start' || start === null || day < start) {
            const keep = fits(end);
            return { start: day, end: keep ? end : day, endpoint: 'end', changed: keep ? ['start'] : ['start', 'end'] };
        }
        if (day - start >= maxDays * DAY) {
            return { start: day, end: day, endpoint: 'end', changed: ['start', 'end'] };
        }
        return { start, end: day, endpoint: 'start', changed: ['end'] };
    }

    // Months reachable by navigation: those with retained data, plus the draft.
    function monthBounds() {
        const months = [month, start, end].filter(day => day !== null && day !== undefined).map(monthOf);
        const bounds = dataBounds();
        if (bounds) {
            for (const value of [bounds.first, bounds.last]) {
                if (Number.isFinite(readDay(value))) months.push(monthOf(readDay(value)));
            }
        }
        return { first: Math.min(...months), last: Math.max(...months) };
    }

    function paint(range = { start, end }) {
        const low = range.start === null ? null : Math.min(range.start, range.end ?? range.start);
        const high = range.start === null ? null : Math.max(range.start, range.end ?? range.start);
        root.querySelectorAll('[data-day]').forEach(cell => {
            const day = Number(cell.dataset.day);
            const selected = low !== null && day >= low && day <= high;
            cell.classList.toggle('in-range', selected);
            cell.classList.toggle('range-start', selected && day === low);
            cell.classList.toggle('range-end', selected && day === high);
            cell.setAttribute('aria-pressed', String(selected));
        });
    }

    function setEndpoint(next) {
        endpoint = next;
        const hint = root.querySelector('.range-calendar-hint');
        if (hint) hint.textContent = translate(endpoint === 'start' ? 'pick_start_day' : 'pick_end_day');
        onEndpointChange(endpoint);
    }

    function select(day) {
        const next = choose(day);
        start = next.start;
        end = next.end;
        focusedDay = day;
        onSelect(key(start), key(end), next.changed);
        setEndpoint(next.endpoint);
        render(true);
    }

    // Rendering replaces every node, so focus returns to the matching control.
    function render(focus = root.contains(document.activeElement)) {
        const control = focus ? ['calendar-prev', 'calendar-next', 'range-calendar-month']
            .find(name => document.activeElement?.classList.contains(name)) : null;
        root.replaceChildren();
        const monthTitle = new Intl.DateTimeFormat(locale(), { month: 'long', year: 'numeric', timeZone: 'UTC' });
        const dayName = new Intl.DateTimeFormat(locale(), { weekday: 'short', timeZone: 'UTC' });
        const fullDate = new Intl.DateTimeFormat(locale(), { dateStyle: 'full', timeZone: 'UTC' });
        const bounds = monthBounds();

        const navigate = target => {
            if (target < bounds.first || target > bounds.last) return;
            month = target;
            let firstAvailable = month;
            while (monthOf(firstAvailable) === month && !isDayAvailable(key(firstAvailable))) firstAvailable += DAY;
            focusedDay = start !== null && monthOf(start) === month ? start
                : end !== null && monthOf(end) === month ? end
                : monthOf(firstAvailable) === month ? firstAvailable : month;
            render(true);
        };
        const nav = element('div', 'range-calendar-nav');
        const previous = button('time-btn calendar-prev', '‹', translate('previous_month'),
            () => navigate(shiftMonths(month, -1)));
        previous.disabled = month <= bounds.first;
        const next = button('time-btn calendar-next', '›', translate('next_month'),
            () => navigate(shiftMonths(month, 1)));
        next.disabled = month >= bounds.last;
        const monthSelect = element('select', 'range-calendar-month');
        monthSelect.setAttribute('aria-label', translate('calendar_month'));
        for (let option = bounds.first; option <= bounds.last; option = shiftMonths(option, 1)) {
            const item = element('option', '', monthTitle.format(option));
            item.value = key(option).slice(0, 7);
            item.selected = option === month;
            monthSelect.append(item);
        }
        monthSelect.addEventListener('change', () => navigate(readDay(`${monthSelect.value}-01`)));
        nav.append(previous, monthSelect, next);

        const hint = element('p', 'range-calendar-hint', translate(endpoint === 'start' ? 'pick_start_day' : 'pick_end_day'));
        hint.setAttribute('role', 'status');

        const grid = element('div', 'range-calendar-grid');
        grid.setAttribute('role', 'group');
        grid.setAttribute('aria-label', monthTitle.format(month));
        // Monday first, over six fixed weeks so the height never jumps.
        for (let i = 0; i < 7; i++) {
            const weekday = element('span', 'range-calendar-weekday', dayName.format(Date.UTC(2026, 5, 1 + i)));
            weekday.setAttribute('aria-hidden', 'true');
            grid.append(weekday);
        }
        const todayKey = today();
        const first = month - ((new Date(month).getUTCDay() + 6) % 7) * DAY;
        for (let i = 0; i < 42; i++) {
            const day = first + i * DAY;
            const cell = element('button', 'range-calendar-day', String(new Date(day).getUTCDate()));
            cell.type = 'button';
            cell.dataset.day = String(day);
            cell.dataset.date = key(day);
            cell.setAttribute('aria-label', fullDate.format(day));
            // aria-disabled keeps days without history focusable, so arrow keys
            // can cross gaps in retention.
            if (!isDayAvailable(key(day))) cell.setAttribute('aria-disabled', 'true');
            if (monthOf(day) !== month) cell.classList.add('outside');
            if (key(day) === todayKey) {
                cell.classList.add('today');
                cell.setAttribute('aria-current', 'date');
            }
            cell.tabIndex = day === focusedDay ? 0 : -1;
            grid.append(cell);
        }
        root.append(nav, hint, grid);
        if (!root.querySelector('[data-day][tabindex="0"]')) {
            (root.querySelector('[data-day]:not(.outside):not([aria-disabled])') ||
                root.querySelector('[data-day]:not(.outside)')).tabIndex = 0;
        }
        paint();
        if (focus) {
            const target = control && root.querySelector(`.${control}`);
            (target && !target.disabled ? target : root.querySelector('[data-day][tabindex="0"]')).focus();
        }
    }

    root.addEventListener('click', event => {
        const cell = event.target.closest('[data-day]');
        if (cell && !unavailable(cell)) select(Number(cell.dataset.day));
    });
    // While choosing the end, hovering or focusing a day previews the result.
    const preview = event => {
        const cell = event.target.closest?.('[data-day]');
        if (endpoint === 'end' && cell && !unavailable(cell)) paint(choose(Number(cell.dataset.day)));
        else paint();
    };
    root.addEventListener('pointerover', preview);
    root.addEventListener('focusin', preview);
    root.addEventListener('pointerleave', () => paint());
    root.addEventListener('keydown', event => {
        const cell = event.target.closest('[data-day]');
        if (!cell) return;
        const day = Number(cell.dataset.day);
        const weekday = (new Date(day).getUTCDay() + 6) % 7;
        // Horizontal arrows follow the visual order, which RTL mirrors.
        const forward = getComputedStyle(root).direction === 'rtl' ? -1 : 1;
        const target = {
            ArrowLeft: day - forward * DAY,
            ArrowRight: day + forward * DAY,
            ArrowUp: day - 7 * DAY,
            ArrowDown: day + 7 * DAY,
            Home: day - weekday * DAY,
            End: day + (6 - weekday) * DAY,
            PageUp: shiftMonths(day, event.shiftKey ? -12 : -1),
            PageDown: shiftMonths(day, event.shiftKey ? 12 : 1),
        }[event.key];
        if (target === undefined) return;
        event.preventDefault();
        const bounds = monthBounds();
        if (monthOf(target) < bounds.first || monthOf(target) > bounds.last) return;
        focusedDay = target;
        month = monthOf(target);
        render(true);
    });

    return {
        // Re-reads the draft. `reset` also returns to the start's month and
        // makes the next click choose the start again.
        sync({ reset = false } = {}) {
            const range = getRange();
            start = dayOrNull(range.start);
            end = dayOrNull(range.end);
            if (reset || month === undefined) {
                month = monthOf(start ?? readDay(today()));
                focusedDay = undefined;
                setEndpoint('start');
            }
            if (focusedDay === undefined || monthOf(focusedDay) !== month) {
                focusedDay = start !== null && monthOf(start) === month ? start : month;
            }
            render();
        },
        // Shows one end of the draft and makes the next click set it.
        editEndpoint(which) {
            const day = which === 'start' ? start : end;
            setEndpoint(which);
            if (day !== null) {
                month = monthOf(day);
                focusedDay = day;
            }
            render(true);
        },
        focus() {
            root.querySelector('[data-day][tabindex="0"]')?.focus();
        },
    };
}

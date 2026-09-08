// Calendar days use UTC only as a timezone-neutral representation. The caller
// converts the chosen day strings to instants in the selected display zone.
const DAY = 86400000;
const key = day => new Date(day).toISOString().slice(0, 10);
const readDay = value => /^\d{4}-\d{2}-\d{2}/.test(value) ? Date.parse(value.slice(0, 10) + 'T00:00:00Z') : NaN;

export function attachRangeCalendar(root, { fromInput, toInput, onSelect, translate, locale, isDayAvailable = () => true }) {
    let month;
    let start = null;
    let end = null;
    let choosingEnd = false;
    let focusedDay;
    const monthOf = day => {
        const date = new Date(day);
        return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1);
    };
    const shiftedMonth = offset => {
        const date = new Date(month);
        return Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + offset, 1);
    };
    const element = (tag, className, text) => {
        const node = document.createElement(tag);
        if (className) node.className = className;
        if (text !== undefined) node.textContent = text;
        return node;
    };
    const button = (text, label, action) => {
        const node = element('button', 'time-btn', text);
        node.type = 'button';
        node.setAttribute('aria-label', label);
        node.addEventListener('click', action);
        return node;
    };

    function paintRange(previewEnd = null) {
        const last = previewEnd ?? end ?? start;
        const low = Math.min(start, last);
        const high = Math.max(start, last);
        root.querySelectorAll('[data-day]').forEach(cell => {
            const day = Number(cell.dataset.day);
            const selected = start !== null && day >= low && day <= high;
            cell.classList.toggle('in-range', selected);
            cell.classList.toggle('range-edge', selected && (day === low || day === high));
            cell.setAttribute('aria-pressed', String(selected));
        });
    }

    function render(focus = false) {
        root.replaceChildren();
        const date = new Date(month);
        const monthName = new Intl.DateTimeFormat(locale(), { month: 'long', timeZone: 'UTC' });
        const monthTitle = new Intl.DateTimeFormat(locale(), { month: 'long', year: 'numeric', timeZone: 'UTC' });
        const dayName = new Intl.DateTimeFormat(locale(), { weekday: 'short', timeZone: 'UTC' });
        const fullDate = new Intl.DateTimeFormat(locale(), { dateStyle: 'full', timeZone: 'UTC' });
        const header = element('div', 'range-calendar-nav');
        const monthSelect = element('select', 'time-input');
        monthSelect.setAttribute('aria-label', translate('calendar_month'));
        for (let i = 0; i < 12; i++) {
            const option = element('option', '', monthName.format(Date.UTC(2026, i, 1)));
            option.value = String(i);
            option.selected = i === date.getUTCMonth();
            monthSelect.append(option);
        }
        const year = element('input', 'time-input range-calendar-year');
        year.type = 'number';
        year.min = '100';
        year.max = '9998';
        year.value = String(date.getUTCFullYear());
        year.setAttribute('aria-label', translate('calendar_year'));
        const navigate = offset => {
            const next = shiftedMonth(offset);
            if (new Date(next).getUTCFullYear() < 100 || new Date(next).getUTCFullYear() > 9998) return;
            month = next;
            focusedDay = month;
            render();
            root.querySelector(offset < 0 ? '.calendar-prev' : '.calendar-next')?.focus();
        };
        const previous = button('‹', translate('previous_month'), () => navigate(-1));
        previous.classList.add('calendar-prev');
        const next = button('›', translate('next_month'), () => navigate(1));
        next.classList.add('calendar-next');
        monthSelect.addEventListener('change', () => {
            month = Date.UTC(date.getUTCFullYear(), Number(monthSelect.value), 1);
            focusedDay = month;
            render();
            root.querySelector('select').focus();
        });
        year.addEventListener('change', () => {
            const value = Number(year.value);
            if (!Number.isInteger(value) || value < 100 || value > 9998) return;
            month = Date.UTC(value, date.getUTCMonth(), 1);
            focusedDay = month;
            render();
            root.querySelector('.range-calendar-year').focus();
        });
        header.append(previous, monthSelect, year, next);
        const hint = element('p', 'time-custom-summary', translate(choosingEnd ? 'pick_end_day' : 'pick_start_day'));
        hint.setAttribute('role', 'status');
        const months = element('div', 'range-calendar-months');
        for (let offset = 0; offset < 2; offset++) {
            const first = shiftedMonth(offset);
            const last = shiftedMonth(offset + 1);
            const title = monthTitle.format(first);
            const panel = element('section', 'range-calendar-month');
            panel.setAttribute('aria-label', title);
            panel.append(element('h4', '', title));
            const grid = element('div', 'range-calendar-grid');
            // Monday first; day arithmetic also works across leap years.
            for (let i = 0; i < 7; i++) {
                grid.append(element('span', 'range-calendar-weekday', dayName.format(Date.UTC(2026, 5, 1 + i))));
            }
            const empty = (new Date(first).getUTCDay() + 6) % 7;
            for (let i = 0; i < empty; i++) grid.append(element('span'));
            for (let day = first; day < last; day += DAY) {
                const label = fullDate.format(day);
                const cell = button(String(new Date(day).getUTCDate()), label, () => {
                    focusedDay = day;
                    if (!choosingEnd) {
                        start = day;
                        end = null;
                        choosingEnd = true;
                    } else {
                        end = Math.max(start, day);
                        start = Math.min(start, day);
                        choosingEnd = false;
                        onSelect(key(start), key(end));
                    }
                    render(true);
                });
                cell.className = 'range-calendar-day';
				cell.dataset.day = String(day);
				cell.dataset.date = key(day);
				cell.disabled = !isDayAvailable(key(day));
				cell.tabIndex = day === focusedDay && !cell.disabled ? 0 : -1;
                cell.addEventListener('pointerenter', () => { if (choosingEnd) paintRange(day); });
                grid.append(cell);
            }
            panel.append(grid);
            months.append(panel);
        }
        root.append(header, hint, months);
        if (!root.querySelector('[data-day]:not(:disabled)[tabindex="0"]')) {
            const firstAvailable = root.querySelector('[data-day]:not(:disabled)');
            if (firstAvailable) firstAvailable.tabIndex = 0;
        }
        paintRange();
        if (focus) root.querySelector(`[data-day="${focusedDay}"]`)?.focus();
    }

    root.addEventListener('pointerleave', () => paintRange());
    root.addEventListener('keydown', event => {
        const cell = event.target.closest('[data-day]');
        if (!cell) return;
        const day = Number(cell.dataset.day);
        const delta = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7 }[event.key];
        let target = delta === undefined ? null : day + delta * DAY;
        if (event.key === 'Home') target = day - ((new Date(day).getUTCDay() + 6) % 7) * DAY;
        if (event.key === 'End') target = day + (6 - (new Date(day).getUTCDay() + 6) % 7) * DAY;
        if (target === null) return;
        event.preventDefault();
        if (!isDayAvailable(key(target))) return;
        focusedDay = target;
        if (monthOf(target) !== month) month = monthOf(target);
        render(true);
        if (choosingEnd) paintRange(target);
    });

    return {
        sync(resetMonth = false) {
            const from = readDay(fromInput.value);
            const to = readDay(toInput.value);
            start = Number.isFinite(from) ? from : null;
            end = Number.isFinite(to) ? to : null;
            choosingEnd = false;
            if (resetMonth || month === undefined) month = monthOf(start ?? Date.now());
            focusedDay = start !== null && monthOf(start) === month ? start : month;
            render();
        },
        isComplete: () => !choosingEnd,
    };
}

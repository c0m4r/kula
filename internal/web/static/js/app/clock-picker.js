import { clockFace, timeOfDayMs, timeOfDayValue } from './format.js';

const HOUR = 3600000;
const MINUTE = 60000;
const SECOND = 1000;
const SVG = 'http://www.w3.org/2000/svg';
// Dial geometry in viewBox units: labels on an outer ring and, for the
// 24-hour clock's afternoon hours, an inner one.
const CENTER = 100;
const OUTER = 78;
const INNER = 52;

const split = ms => ({
    hour: Math.floor(ms / HOUR),
    minute: Math.floor(ms % HOUR / MINUTE),
    second: Math.floor(ms % MINUTE / SECOND),
    rest: ms % SECOND,
});
const join = ({ hour, minute, second, rest }) => hour * HOUR + minute * MINUTE + second * SECOND + rest;

// The picker's clock for one end of the draft: a dial and scroll wheels that
// stay in step with the time field. The dial sets a unit and zeroes the finer
// ones (click 6 for 06:00:00, then the minutes); a wheel, a scroll or an arrow
// key changes only its own unit. getState returns { title, time, shortcuts };
// picks are reported through onPick as 'HH:MM:SS(.mmm)'.
export function attachClockPicker(root, { getState, onPick, onShortcut, onCalendar, translate, locale }) {
    const title = root.querySelector('.range-clock-title');
    const dial = root.querySelector('.range-dial');
    const wheelHost = root.querySelector('.range-wheels');
    const shortcuts = root.querySelector('.range-clock-shortcuts');
    let face;
    let built = '';
    let mode = 'hour';
    let value = null;
    let wheels = [];

    const svg = (tag, attributes = {}) => {
        const node = document.createElementNS(SVG, tag);
        for (const [name, attribute] of Object.entries(attributes)) node.setAttribute(name, attribute);
        return node;
    };
    const point = (angle, radius) => ({
        x: CENTER + radius * Math.sin(angle * Math.PI / 180),
        y: CENTER - radius * Math.cos(angle * Math.PI / 180),
    });
    const current = () => split(value ?? 0);
    const pick = ms => {
        if (ms !== value) onPick(timeOfDayValue(ms));
    };

    // ---- Wheels ----
    function wheelSpecs() {
        const specs = [
            face.periods ? {
                unit: 'hour', labels: face.hours.slice(0, 12),
                index: ms => split(ms).hour % 12,
                apply: (ms, index) => ({ ...split(ms), hour: index + (split(ms).hour >= 12 ? 12 : 0) }),
            } : {
                unit: 'hour', labels: face.hours,
                index: ms => split(ms).hour,
                apply: (ms, index) => ({ ...split(ms), hour: index }),
            },
            {
                unit: 'minute', labels: face.sixty,
                index: ms => split(ms).minute,
                apply: (ms, index) => ({ ...split(ms), minute: index }),
            },
            {
                unit: 'second', labels: face.sixty,
                index: ms => split(ms).second,
                apply: (ms, index) => ({ ...split(ms), second: index }),
            },
        ];
        if (face.periods) {
            const period = {
                unit: 'period', labels: face.periods, name: face.periods.join('/'),
                index: ms => split(ms).hour >= 12 ? 1 : 0,
                apply: (ms, index) => ({ ...split(ms), hour: split(ms).hour % 12 + index * 12 }),
            };
            if (face.periodFirst) specs.unshift(period);
            else specs.push(period);
        }
        return specs;
    }

    function buildWheels() {
        const spacer = () => {
            const node = document.createElement('div');
            node.className = 'range-wheel-spacer';
            return node;
        };
        wheelHost.replaceChildren();
        wheels = wheelSpecs().map((spec, position, all) => {
            const element = document.createElement('div');
            element.className = 'range-wheel';
            element.tabIndex = 0;
            element.dataset.unit = spec.unit;
            element.setAttribute('role', 'spinbutton');
            element.setAttribute('aria-label', spec.name || translate(spec.unit));
            element.setAttribute('aria-valuemin', '0');
            element.setAttribute('aria-valuemax', String(spec.labels.length - 1));
            element.append(spacer(), ...spec.labels.map((label, index) => {
                const item = document.createElement('div');
                item.className = 'range-wheel-item';
                item.dataset.index = String(index);
                item.textContent = label;
                return item;
            }), spacer());
            for (const node of element.children) node.setAttribute('aria-hidden', 'true');
            const previous = all[position - 1];
            if (previous && previous.unit !== 'period' && spec.unit !== 'period') {
                const separator = document.createElement('span');
                separator.className = 'range-wheel-separator';
                separator.textContent = face.separator;
                separator.setAttribute('aria-hidden', 'true');
                wheelHost.append(separator);
            }
            // + and − step by one; the wheel's own arrow keys do the same, so
            // the buttons stay out of the tab order.
            const stepper = (text, step) => {
                const button = document.createElement('button');
                button.type = 'button';
                button.className = 'range-wheel-step';
                button.textContent = text;
                button.tabIndex = -1;
                button.dataset.step = String(step);
                button.setAttribute('aria-hidden', 'true');
                return button;
            };
            const column = document.createElement('div');
            column.className = 'range-wheel-column';
            column.append(stepper('+', 1), element, stepper('−', -1));
            wheelHost.append(column);
            return { ...spec, element, target: null, stale: true, user: false, scrolled: false, drag: null, delta: 0, timer: 0 };
        });
    }

    const itemHeight = wheel => wheel.element.querySelector('.range-wheel-item').getBoundingClientRect().height;
    const nearest = wheel => {
        const height = itemHeight(wheel);
        const index = height ? Math.round(wheel.element.scrollTop / height) : 0;
        return Math.max(0, Math.min(wheel.labels.length - 1, index));
    };

    function place(wheel, index, smooth) {
        // Long trips, such as wrapping from 59 to 00, jump rather than spin.
        const glide = smooth && wheel.target !== null && Math.abs(index - wheel.target) <= 12;
        wheel.target = index;
        const height = itemHeight(wheel);
        // A closed picker has no layout; place it again once shown.
        wheel.stale = !height;
        if (height) wheel.element.scrollTo({ top: index * height, behavior: glide ? 'smooth' : 'auto' });
    }

    function markWheel(wheel, index) {
        wheel.element.querySelectorAll('.range-wheel-item').forEach(item => {
            item.classList.toggle('current', Number(item.dataset.index) === index);
        });
    }

    // Wheel changes keep the other units. Scrolling stops at the ends; steps
    // wrap around.
    function chooseWheel(wheel, index, wrap = false) {
        const count = wheel.labels.length;
        index = wrap ? (index % count + count) % count : Math.max(0, Math.min(count - 1, index));
        place(wheel, index, true);
        markWheel(wheel, index);
        setMode(wheel.unit === 'period' ? 'hour' : wheel.unit);
        pick(join(wheel.apply(value ?? 0, index)));
    }

    const wheelOf = target => target ? wheels.find(wheel => wheel.element.contains(target)) : undefined;

    // A mouse notch moves one item; touchpad scrolling accumulates.
    wheelHost.addEventListener('wheel', event => {
        const wheel = wheelOf(event.target);
        if (!wheel) return;
        event.preventDefault();
        const pixels = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? 100 : 1);
        let steps = 0;
        if (Math.abs(pixels) >= 50) {
            steps = Math.sign(pixels);
            wheel.delta = 0;
        } else {
            wheel.delta += pixels;
            if (Math.abs(wheel.delta) >= 30) {
                steps = Math.sign(wheel.delta);
                wheel.delta = 0;
            }
        }
        if (steps) chooseWheel(wheel, (wheel.target ?? 0) + steps);
    }, { passive: false });

    // Holding + or − repeats, first after a pause.
    let repeat = 0;
    const stopRepeat = () => clearTimeout(repeat);
    wheelHost.addEventListener('pointerdown', event => {
        const step = event.target.closest('.range-wheel-step');
        if (!step || event.button !== 0) return;
        event.preventDefault();
        const wheel = wheelOf(step.parentElement.querySelector('.range-wheel'));
        wheel.element.focus({ preventScroll: true });
        const once = () => chooseWheel(wheel, (wheel.target ?? 0) + Number(step.dataset.step), true);
        const again = delay => {
            repeat = setTimeout(() => {
                once();
                again(80);
            }, delay);
        };
        stopRepeat();
        once();
        again(400);
    });
    for (const type of ['pointerup', 'pointercancel', 'pointerleave']) {
        wheelHost.addEventListener(type, stopRepeat);
    }
    wheelHost.addEventListener('pointerout', event => {
        if (event.target.closest('.range-wheel-step')) stopRepeat();
    });

    // Touch scrolls natively and snaps; the value is read when it settles.
    // A mouse or pen drags the wheel with snapping paused.
    wheelHost.addEventListener('pointerdown', event => {
        const wheel = wheelOf(event.target);
        if (!wheel) return;
        wheel.user = true;
        wheel.scrolled = false;
        if (event.pointerType === 'touch' || event.button !== 0) return;
        event.preventDefault();
        wheel.element.focus({ preventScroll: true });
        wheel.drag = { id: event.pointerId, y: event.clientY, top: wheel.element.scrollTop, moved: false };
    });
    wheelHost.addEventListener('pointermove', event => {
        const wheel = wheels.find(item => item.drag?.id === event.pointerId);
        if (!wheel) return;
        const offset = event.clientY - wheel.drag.y;
        if (!wheel.drag.moved && Math.abs(offset) > 3) {
            wheel.drag.moved = true;
            // Capturing only once it moves leaves plain clicks on their item.
            wheel.element.setPointerCapture(event.pointerId);
            wheel.element.classList.add('dragging');
        }
        if (wheel.drag.moved) {
            wheel.element.scrollTop = wheel.drag.top - offset;
            markWheel(wheel, nearest(wheel));
        }
    });
    const release = event => {
        const wheel = wheels.find(item => item.drag?.id === event.pointerId || (event.pointerType === 'touch' &&
            item.user && item.element.contains(event.target)));
        if (!wheel) return;
        if (wheel.drag) {
            const moved = wheel.drag.moved;
            wheel.drag = null;
            wheel.element.classList.remove('dragging');
            wheel.user = false;
            if (moved) chooseWheel(wheel, nearest(wheel));
        } else if (event.type === 'pointerup' && !wheel.scrolled) {
            wheel.user = false;
        }
    };
    wheelHost.addEventListener('pointerup', release);
    wheelHost.addEventListener('pointercancel', release);
    wheelHost.addEventListener('click', event => {
        const item = event.target.closest('.range-wheel-item');
        const wheel = item && wheelOf(item);
        if (wheel) chooseWheel(wheel, Number(item.dataset.index));
    });
    wheelHost.addEventListener('focusin', event => {
        const wheel = wheelOf(event.target);
        if (wheel) setMode(wheel.unit === 'period' ? 'hour' : wheel.unit);
    });
    wheelHost.addEventListener('keydown', event => {
        const wheel = wheelOf(event.target);
        if (!wheel) return;
        const index = wheel.target ?? 0;
        const step = { ArrowUp: 1, ArrowDown: -1, PageUp: 5, PageDown: -5 }[event.key];
        const target = step !== undefined ? index + step
            : { Home: 0, End: wheel.labels.length - 1 }[event.key];
        if (target === undefined) return;
        event.preventDefault();
        chooseWheel(wheel, target, step !== undefined);
    });
    const settle = wheel => {
        if (!wheel.user || wheel.drag) return;
        wheel.user = false;
        const index = nearest(wheel);
        if (index !== wheel.target) chooseWheel(wheel, index);
    };
    const settles = 'onscrollend' in window;
    function watchScroll(wheel) {
        wheel.element.addEventListener('scroll', () => {
            if (!wheel.user) return;
            wheel.scrolled = true;
            if (!wheel.drag) markWheel(wheel, nearest(wheel));
            if (settles) return;
            clearTimeout(wheel.timer);
            wheel.timer = setTimeout(() => settle(wheel), 150);
        });
        if (settles) wheel.element.addEventListener('scrollend', () => settle(wheel));
    }

    // ---- Dial ----
    const face12 = () => face.periods !== null;
    function renderDial() {
        const time = current();
        const hourMode = mode === 'hour';
        const labels = [];
        if (hourMode) {
            for (let hour = 0; hour < (face12() ? 12 : 24); hour++) {
                labels.push({ text: face.hours[hour], angle: (hour % 12) * 30, radius: hour < 12 ? OUTER : INNER, inner: hour >= 12 });
            }
        } else {
            for (let minute = 0; minute < 60; minute += 5) {
                labels.push({ text: face.sixty[minute], angle: minute * 6, radius: OUTER, inner: false });
            }
        }
        const selected = hourMode ? {
            angle: (time.hour % 12) * 30,
            radius: !face12() && time.hour >= 12 ? INNER : OUTER,
            text: face.hours[time.hour],
        } : { angle: time[mode] * 6, radius: OUTER, text: face.sixty[time[mode]] };
        const knob = point(selected.angle, selected.radius);
        const graphic = svg('svg', { viewBox: '0 0 200 200', 'aria-hidden': 'true' });
        graphic.append(svg('circle', { class: 'range-dial-plate', cx: CENTER, cy: CENTER, r: 98 }));
        if (value !== null) {
            graphic.append(
                svg('line', { class: 'range-dial-hand', x1: CENTER, y1: CENTER, x2: knob.x, y2: knob.y }),
                svg('circle', { class: 'range-dial-pivot', cx: CENTER, cy: CENTER, r: 3 }),
                svg('circle', { class: 'range-dial-knob', cx: knob.x, cy: knob.y, r: selected.radius === INNER ? 13 : 15 }),
            );
        }
        for (const label of labels) {
            const at = point(label.angle, label.radius);
            const text = svg('text', { class: label.inner ? 'range-dial-label inner' : 'range-dial-label', x: at.x, y: at.y });
            text.textContent = label.text;
            graphic.append(text);
        }
        if (value !== null) {
            const text = svg('text', { class: 'range-dial-label on-knob', x: knob.x, y: knob.y });
            text.textContent = selected.text;
            graphic.append(text);
        }
        dial.replaceChildren(graphic);
        const max = mode === 'hour' ? 23 : 59;
        dial.setAttribute('aria-label', translate(mode));
        dial.setAttribute('aria-valuemax', String(max));
        dial.setAttribute('aria-valuenow', String(time[mode]));
        dial.setAttribute('aria-valuetext', value === null ? '' : face.name(value - value % MINUTE));
        wheels.forEach(wheel => wheel.element.classList.toggle('dial-unit',
            wheel.unit === mode || (wheel.unit === 'period' && mode === 'hour')));
    }

    function setMode(next) {
        if (next === mode) return;
        mode = next;
        renderDial();
    }

    function dialPoint(event) {
        const box = dial.getBoundingClientRect();
        const x = (event.clientX - box.left - box.width / 2) / (box.width / 2) * CENTER;
        const y = (event.clientY - box.top - box.height / 2) / (box.height / 2) * CENTER;
        return { angle: (Math.atan2(x, -y) * 180 / Math.PI + 360) % 360, distance: Math.hypot(x, y) };
    }

    function pickOnDial(event) {
        const { angle, distance } = dialPoint(event);
        const time = current();
        if (mode === 'hour') {
            const position = Math.round(angle / 30) % 12;
            const afternoon = face12() ? time.hour >= 12 : distance < (OUTER + INNER) / 2;
            pick(join({ hour: position + (afternoon ? 12 : 0), minute: 0, second: 0, rest: 0 }));
        } else {
            const unit = Math.round(angle / 6) % 60;
            pick(join({ ...time, [mode]: unit, ...(mode === 'minute' ? { second: 0 } : {}), rest: 0 }));
        }
    }

    let dialPointer = null;
    dial.addEventListener('pointerdown', event => {
        if (event.button !== 0) return;
        event.preventDefault();
        dial.focus({ preventScroll: true });
        dial.setPointerCapture(event.pointerId);
        dialPointer = event.pointerId;
        pickOnDial(event);
    });
    dial.addEventListener('pointermove', event => {
        if (event.pointerId === dialPointer) pickOnDial(event);
    });
    const endDial = event => {
        if (event.pointerId !== dialPointer) return;
        dialPointer = null;
        // Like a phone's clock: after the hour come the minutes.
        if (event.type === 'pointerup' && mode === 'hour') setMode('minute');
    };
    dial.addEventListener('pointerup', endDial);
    dial.addEventListener('pointercancel', endDial);
    // The dial is a slider over the unit it shows; keys keep the other units.
    dial.addEventListener('keydown', event => {
        const time = current();
        const count = mode === 'hour' ? 24 : 60;
        const step = {
            ArrowUp: 1, ArrowRight: 1, ArrowDown: -1, ArrowLeft: -1,
            PageUp: mode === 'hour' ? 6 : 5, PageDown: mode === 'hour' ? -6 : -5,
        }[event.key];
        let next;
        if (step !== undefined) next = (time[mode] + step + count) % count;
        else if (event.key === 'Home') next = 0;
        else if (event.key === 'End') next = count - 1;
        else if (event.key === 'Enter') {
            event.preventDefault();
            setMode({ hour: 'minute', minute: 'second', second: 'hour' }[mode]);
            return;
        } else return;
        event.preventDefault();
        pick(join({ ...time, [mode]: next }));
    });

    // ---- Whole pane ----
    function build(view) {
        face = clockFace(locale());
        buildWheels();
        wheels.forEach(watchScroll);
        shortcuts.replaceChildren(...view.shortcuts.map(id => {
            const shortcut = document.createElement('button');
            shortcut.type = 'button';
            shortcut.className = 'time-btn';
            shortcut.dataset.shortcut = id;
            shortcut.textContent = translate(id);
            return shortcut;
        }));
        built = `${locale()}|${view.shortcuts}`;
    }

    // `reset` shows the hours again and places the wheels without animation,
    // for a pane that has just appeared.
    function update({ reset = false } = {}) {
        const view = getState();
        if (built !== `${locale()}|${view.shortcuts}`) build(view);
        if (reset) mode = 'hour';
        title.textContent = view.title;
        value = timeOfDayMs(view.time);
        for (const wheel of wheels) {
            const index = value === null ? null : wheel.index(value);
            if (!wheel.user) markWheel(wheel, index);
            wheel.element.setAttribute('aria-valuenow', String(index ?? 0));
            wheel.element.setAttribute('aria-valuetext', index === null ? '' : wheel.labels[index]);
            if (index === null || wheel.user) continue;
            if (reset || wheel.stale) place(wheel, index, false);
            else if (index !== wheel.target) place(wheel, index, true);
        }
        renderDial();
    }

    root.addEventListener('click', event => {
        const target = event.target.closest('button');
        if (target?.classList.contains('range-clock-back')) onCalendar();
        else if (target?.dataset.shortcut) onShortcut(target.dataset.shortcut);
    });

    return { update };
}

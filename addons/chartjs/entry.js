/* ============================================================
   entry.js — Kula's Chart.js bundle: only what the dashboard's
   line charts use. Built by addons/build-chartjs.sh into
   internal/web/static/js/chartjs/chartjs-bundle.min.js.
   ============================================================ */
'use strict';
import {
    Chart,
    Legend,
    LinearScale,
    LineController,
    LineElement,
    PointElement,
    TimeScale,
    Tooltip,
    _adapters,
} from 'chart.js';
import zoomPlugin from 'chartjs-plugin-zoom';
import { dateAdapter } from './date-adapter.js';

_adapters._date.override(dateAdapter);
Chart.register(LineController, LineElement, PointElement, LinearScale, TimeScale, Legend, Tooltip, zoomPlugin);
// state.js adds a tooltip positioner through Chart.Tooltip.
Chart.Tooltip = Tooltip;
globalThis.Chart = Chart;

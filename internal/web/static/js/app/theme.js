/* ============================================================
   theme.js — Theme application (dark/light) and toggling.
   ============================================================ */
'use strict';

function applyTheme() {
    const isLight = state.theme === 'light';
    document.body.classList.toggle('light-mode', isLight);

    // Update Chart.js defaults for future charts (if any re-init)
    const gridColor = isLight ? 'rgba(203, 213, 225, 0.4)' : 'rgba(55, 65, 81, 0.2)';
    const textColor = isLight ? '#64748b' : '#94a3b8';
    const tooltipBg = isLight ? 'rgba(255, 255, 255, 0.95)' : 'rgba(17, 24, 39, 0.9)';
    const tooltipText = isLight ? '#1e293b' : '#f1f5f9';
    const tooltipBorder = isLight ? 'rgba(203, 213, 225, 0.8)' : 'rgba(55, 65, 81, 0.5)';

    Chart.defaults.color = textColor;
    Chart.defaults.borderColor = gridColor;

    // Update default tooltip colors
    Chart.defaults.plugins.tooltip.backgroundColor = tooltipBg;
    Chart.defaults.plugins.tooltip.titleColor = tooltipText;
    Chart.defaults.plugins.tooltip.bodyColor = tooltipText;
    Chart.defaults.plugins.tooltip.borderColor = tooltipBorder;
    Chart.defaults.plugins.tooltip.borderWidth = 1;

    // Update existing charts
    Object.values(state.charts).forEach(chart => {
        if (!chart) return;
        if (chart.options.scales.x) {
            chart.options.scales.x.grid.color = gridColor;
            chart.options.scales.x.ticks.color = textColor;
        }
        if (chart.options.scales.y) {
            chart.options.scales.y.grid.color = gridColor;
            chart.options.scales.y.ticks.color = textColor;
        }
        if (chart.options.scales.y1) {
            chart.options.scales.y1.grid.color = gridColor;
            chart.options.scales.y1.ticks.color = textColor;
        }
        if (chart.options.plugins.tooltip) {
            chart.options.plugins.tooltip.backgroundColor = tooltipBg;
            chart.options.plugins.tooltip.titleColor = tooltipText;
            chart.options.plugins.tooltip.bodyColor = tooltipText;
            chart.options.plugins.tooltip.borderColor = tooltipBorder;
            chart.options.plugins.tooltip.borderWidth = 1;
        }
        chart.update('none');
    });
}

function toggleTheme() {
    state.theme = state.theme === 'dark' ? 'light' : 'dark';
    localStorage.setItem('kula_theme', state.theme);
    applyTheme();
}

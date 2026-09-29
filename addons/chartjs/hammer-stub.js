// chartjs-plugin-zoom skips Hammer.js when it is absent. Kula drives pan and
// pinch with Pointer Events (chart-interactions.js), so the bundle omits it.
export default undefined;

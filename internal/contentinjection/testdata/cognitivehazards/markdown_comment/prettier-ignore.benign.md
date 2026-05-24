# Lint-directive control

<!-- prettier-ignore -->
```js
const x = 1;
```

<!-- prettier-ignore-start -->
const formatted = "intentionally weird formatting";
<!-- prettier-ignore-end -->

These directive comments contain the catalog word "ignore" but are
below the 32-char body-length threshold AND have no verb at first
position. The detector must not fire — this is the design doc's
"score on length × verb density, not bare presence" rule.

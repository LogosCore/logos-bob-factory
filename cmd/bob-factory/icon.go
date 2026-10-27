package main

// icon is BOB's icon in core's UI, sent at registration: the boxing dummy's
// head and neck in outline, a square, muscular face with no expression. Kept
// in the code, where a factory that draws its icon per build or reads it from
// a file would do that instead.
const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <title>BOB</title>
  <path d="M6 7.2C6 4.2 8.3 2 12 2S18 4.2 18 7.2V13.4C18 14 17.8 14.6 17.5 15.1L15.8 17.2C15.4 17.6 14.9 17.8 14.3 17.8H9.7C9.1 17.8 8.6 17.6 8.2 17.2L6.5 15.1C6.2 14.6 6 14 6 13.4Z"/>
  <path d="M6 10.2C5.3 10.2 5 10.7 5 11.3S5.3 12.4 6 12.4M18 10.2C18.7 10.2 19 10.7 19 11.3S18.7 12.4 18 12.4"/>
  <path d="M8.6 17.6V24M15.4 17.6V24" stroke-linecap="butt"/>
  <path d="M9.7 10.1h.01M14.3 10.1h.01 M8.7 8.1h2.1M13.2 8.1h2.1 M10.6 15.1h2.8 M12 9.4v2.9M11.2 12.6h1.6" stroke-width="1.6"/>
</svg>`

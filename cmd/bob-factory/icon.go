package main

// icon is BOB's icon in core's UI, sent at registration: a stick man in a
// boxing stance, jabbing with his gloves on. Kept in the code, where a factory
// that draws its icon per build or reads it from a file would do that instead.
const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <title>BOB</title>
  <circle cx="9.4" cy="3.9" r="1.9"/>
  <path d="M9.7 6.9L9 13.8M9.6 8.6L14.2 10.4L18 8.9M9.6 8.6L11.4 11L13.4 5.6M9 13.8L12.4 17.2L13.6 21.6M9 13.8L7.2 17.6L4.4 21.6"/>
  <circle cx="18.8" cy="8.6" r="1.7" fill="currentColor" stroke="none"/>
  <circle cx="13.7" cy="4.8" r="1.7" fill="currentColor" stroke="none"/>
</svg>`

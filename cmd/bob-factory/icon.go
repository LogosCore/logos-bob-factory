package main

// icon is BOB's icon in core's UI, sent at registration: the boxing dummy,
// head, torso, post and base. Kept in the code, where a factory that draws its
// icon per build or reads it from a file would do that instead.
const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <title>BOB</title>
  <circle cx="12" cy="5" r="2.5"/>
  <path d="M7 11a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v3a5 5 0 0 1-10 0z"/>
  <path d="M12 19v3M8 22h8"/>
</svg>`

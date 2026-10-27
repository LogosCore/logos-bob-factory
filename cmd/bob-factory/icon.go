package main

// icon is BOB's icon in core's UI, sent at registration: the boxing dummy's
// bust, bald and stern, with its thick neck and the line of its chest. The
// face and chest are cut out of a solid shape, which reads at 16 px better than
// strokes. Kept in the code, where a factory that draws its icon per build or
// reads it from a file would do that instead.
const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor">
  <title>BOB</title>
  <path fill-rule="evenodd" d="M12 1C15.7 1 17.6 3.5 17.6 7.1C17.6 8.5 17.4 9.7 17 10.8L16.2 12.5C15.7 13.5 14.7 14.2 13.6 14.4H10.4C9.3 14.2 8.3 13.5 7.8 12.5L7 10.8C6.6 9.7 6.4 8.5 6.4 7.1C6.4 3.5 8.3 1 12 1Z M8.5 7.6L11.1 8L10.95 9.05L8.7 8.75Z M15.5 7.6L12.9 8L13.05 9.05L15.3 8.75Z M10 12H14V12.75H10Z M11.25 10.35H12.75Q12.75 10.85 12 10.85Q11.25 10.85 11.25 10.35Z"/>
  <ellipse cx="6.35" cy="8.4" rx="0.9" ry="1.5"/>
  <ellipse cx="17.65" cy="8.4" rx="0.9" ry="1.5"/>
  <path fill-rule="evenodd" d="M8.8 13V14.9C7 15.3 5 15.9 3.5 17C2.1 18 1.5 19.6 1.5 21.4V23H22.5V21.4C22.5 19.6 21.9 18 20.5 17C19 15.9 17 15.3 15.2 14.9V13Z M11.8 18.2H12.2V20.9H11.8Z M5.8 20.6Q8.8 21.8 11.5 20.8V21.25Q8.8 22.3 5.8 21.05Z M18.2 20.6Q15.2 21.8 12.5 20.8V21.25Q15.2 22.3 18.2 21.05Z"/>
</svg>`

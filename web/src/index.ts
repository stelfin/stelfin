(() => {
  "use strict";

  // Reveal sections as they scroll into view. Purely presentational — unlike
  // confirm.js/enroll.js, nothing here reads or writes a signing key, calls
  // the API, or carries any authority. That's what makes this page safe to
  // build with a compile step confirm.js/enroll.js deliberately don't have:
  // there is nothing here a compromised build pipeline could get signed.
  const sections = document.querySelectorAll<HTMLElement>("[data-reveal]");

  if (!("IntersectionObserver" in window) || sections.length === 0) {
    sections.forEach((el) => el.classList.add("is-visible"));
    return;
  }

  const observer = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          entry.target.classList.add("is-visible");
          observer.unobserve(entry.target);
        }
      }
    },
    { threshold: 0.15 },
  );

  sections.forEach((el) => observer.observe(el));
})();

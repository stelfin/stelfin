"use strict";
(() => {
    "use strict";
    // Everything below is presentational. Unlike confirm.js/enroll.js, nothing
    // here reads or writes a signing key, calls the API, or carries any
    // authority — that's what makes this page safe to build with a compile
    // step confirm.js/enroll.js deliberately don't have: there is nothing here
    // a compromised build pipeline could get signed.
    // --- Reveal sections as they scroll into view ---
    const sections = document.querySelectorAll("[data-reveal]");
    if ("IntersectionObserver" in window && sections.length > 0) {
        const revealObserver = new IntersectionObserver((entries) => {
            for (const entry of entries) {
                if (entry.isIntersecting) {
                    entry.target.classList.add("is-visible");
                    revealObserver.unobserve(entry.target);
                }
            }
        }, { threshold: 0.15 });
        sections.forEach((el) => revealObserver.observe(el));
    }
    else {
        sections.forEach((el) => el.classList.add("is-visible"));
    }
    // --- Sticky navbar gains a background once the page has scrolled ---
    // A sentinel at the very top rather than a scroll listener: cheaper, and
    // the observer only fires on the transition instead of every scroll tick.
    const navSentinel = document.getElementById("nav-sentinel");
    const nav = document.querySelector("[data-nav]");
    if (navSentinel && nav && "IntersectionObserver" in window) {
        const navObserver = new IntersectionObserver((entries) => {
            const entry = entries[0];
            if (entry)
                nav.classList.toggle("is-scrolled", !entry.isIntersecting);
        }, { threshold: 0 });
        navObserver.observe(navSentinel);
    }
    // --- FAQ accordion: one open at a time, no framework ---
    // Height animates via a CSS grid-template-rows 0fr/1fr trick rather than
    // measuring scrollHeight in JS, so this stays a class toggle either way.
    const faqButtons = document.querySelectorAll("[data-faq-toggle]");
    faqButtons.forEach((button) => {
        button.addEventListener("click", () => {
            const item = button.closest("[data-faq-item]");
            if (!item)
                return;
            const wasOpen = item.classList.contains("is-open");
            document.querySelectorAll("[data-faq-item].is-open").forEach((open) => {
                open.classList.remove("is-open");
                open.querySelector("[data-faq-toggle]")?.setAttribute("aria-expanded", "false");
            });
            if (!wasOpen) {
                item.classList.add("is-open");
                button.setAttribute("aria-expanded", "true");
            }
        });
    });
})();

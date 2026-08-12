// Project picker dropdown in the top bar: a details/summary with one checkbox
// per registered project. The app hands over the registry and the current
// selection via data and hears about changes through data.onChange; opening
// the picker collapses the nav dropdown through data.onOpen (the toggle event
// does not bubble, so the element reports it itself).

import { escapeAttr, escapeHTML } from "../html.js";
import { value } from "../normalize.js";
import { define, FlowElement } from "./base.js";

// renderProjectPickerMenu is the picker's whole markup: hidden (an empty
// string) unless the registry holds more than one project.
export function renderProjectPickerMenu({ projects = [], selected = [] } = {}) {
  if (projects.length <= 1) return "";
  const selectedSet = new Set(selected);
  const summary = selected.length ? `Projects: ${selected.length}/${projects.length}` : "Projects: All";
  return `
    <details class="project-picker">
      <summary class="button secondary">${escapeHTML(summary)}</summary>
      <div class="project-picker-menu">
        <label class="project-picker-item"><input type="checkbox" data-project-all ${selected.length ? "" : "checked"}><span>All projects</span></label>
        ${projects.map((project) => {
          const id = value(project, "id", "ID");
          const name = value(project, "name", "Name") || id;
          const checked = !selected.length || selectedSet.has(id);
          return `<label class="project-picker-item"><input type="checkbox" data-project-option="${escapeAttr(id)}" ${checked ? "checked" : ""}><span>${escapeHTML(name)}</span></label>`;
        }).join("")}
      </div>
    </details>
  `;
}

export class FlowProjectPicker extends FlowElement {
  // data: {
  //   projects: [],          // the project registry
  //   selected: [],          // the active selection ([] = all projects)
  //   onChange(ids),         // a checkbox changed; [] means All projects
  //   onOpen(),              // the dropdown opened (app closes the nav menu)
  // }
  render() {
    return renderProjectPickerMenu(this.data || {});
  }

  bind() {
    // Delegated on the element so it survives the element replacing its own
    // innerHTML on repaint.
    this.addEventListener("change", (event) => {
      if (event.target?.closest?.("[data-project-all]")) {
        this.data?.onChange?.([]);
        return;
      }
      if (!event.target?.closest?.("[data-project-option]")) return;
      const ids = Array.from(this.querySelectorAll("[data-project-option]"))
        .filter((option) => option.checked)
        .map((option) => option.dataset.projectOption);
      this.data?.onChange?.(ids);
    });
  }

  afterPaint() {
    const details = this.querySelector("details");
    // An empty registry renders nothing; keep the element hidden so the
    // top-bar slot collapses until data arrives (and again if it empties).
    this.toggleAttribute("hidden", !details);
    details?.addEventListener("toggle", () => {
      if (details.open) this.data?.onOpen?.();
    });
  }

  // close collapses the dropdown; the nav menu calls this when it opens so
  // only one top-bar menu is open at a time.
  close() {
    const details = this.querySelector("details");
    if (details) details.open = false;
  }
}

define("flow-project-picker", FlowProjectPicker);

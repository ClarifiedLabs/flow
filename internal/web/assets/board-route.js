// The board route: fetch, project, hand to <flow-board>.
//
// The route no longer writes markup. It mounts the element once and then only
// sets its `data`, so a poll updates the cards in place instead of rebuilding
// the page — which is what lets a card keep its hover, its focus and its
// scroll position while the numbers on it change.

import { apiGet } from "./api.js";
import { boardEntries } from "./elements/board.js";
import { mount } from "./elements/base.js";
import { boardDoneQueryView } from "./board-view.js";

export async function renderBoardRoute(app, context) {
  const [data, doneData] = await Promise.all([
    apiGet("/v2/board" + app.projectQuery()),
    apiGet("/v2/done" + boardDoneQueryView(app)).catch(() => null),
  ]);
  if (context && !app.isActiveLoad(context)) return false;

  app.setTitle("Board");
  const showProject = (app.projects || []).length > 1;
  const entries = boardEntries(data, { showProject });
  const board = mount(app.querySelector(".content"), "flow-board", { entries, doneData, showProject });

  const attention = entries.filter((entry) => entry.model.needsYou).length;
  app.setStatus(
    attention
      ? `${entries.length} tasks · ${attention} waiting on you`
      : `${entries.length} tasks · nothing waiting on you`,
  );
  return Boolean(board);
}

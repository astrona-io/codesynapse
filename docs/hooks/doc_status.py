"""Mark every docs page as a draft until a person has read and verified it.

Pages are written and updated with AI assistants while the software is being
built, so none of them is trusted by default. A page counts as verified only
when its front matter says so:

    ---
    verified: true
    ---

Every other page gets Material's "draft" page status (an icon in the
navigation, see extra.status in mkdocs.yml) and a Draft badge under its title.

The key is `verified`, not `status`: front matter `status:` is already the
record status the architecture catalog reads from ADRs (internal/arch), and a
docs flag there would overwrite it.
"""

import re

from mkdocs.exceptions import PluginError

_BADGE = (
    '<p class="cs-doc-status" data-search-exclude>'
    '<span class="cs-doc-status__label">Draft</span> '
    "Not yet reviewed by a maintainer. It may be incomplete or out of date."
    "</p>"
)
_FIRST_H1 = re.compile(r"</h1>", re.IGNORECASE)


def _verified(page):
    value = page.meta.get("verified", False)
    if not isinstance(value, bool):
        raise PluginError(
            f"{page.file.src_uri}: front matter 'verified' must be true or false, got {value!r}"
        )
    status = page.meta.get("status")
    if isinstance(status, str) and status.strip().lower() in ("draft", "verified"):
        raise PluginError(
            f"{page.file.src_uri}: use 'verified: true' (or leave it out for a draft), "
            f"not 'status: {status}'"
        )
    return value


def on_page_markdown(markdown, page, config, files):
    # Material reads page.meta["status"] for the navigation icon. A page's own
    # `status:` (an ADR's accepted/proposed) is not a page status Material
    # knows, so it is replaced here either way.
    if _verified(page):
        page.meta.pop("status", None)
    else:
        page.meta["status"] = "draft"
    return markdown


def on_page_content(html, page, config, files):
    if page.meta.get("status") != "draft":
        return html
    # Right under the title; a page without an <h1> gets Material's generated
    # one first, so the badge goes on top of the content instead.
    if _FIRST_H1.search(html):
        return _FIRST_H1.sub("</h1>" + _BADGE, html, count=1)
    return _BADGE + html

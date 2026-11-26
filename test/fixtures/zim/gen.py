# Generates the tiny ZIM fixtures: uvx --with libzim==4.1.0 python -I gen.py
import json
import pathlib

from libzim.reader import Archive
from libzim.writer import Creator, Hint, Item, StringProvider


class Page(Item):
    def __init__(self, path, title, content):
        super().__init__()
        self.path, self.title, self.content = path, title, content

    def get_path(self):
        return self.path

    def get_title(self):
        return self.title

    def get_mimetype(self):
        return "text/html"

    def get_contentprovider(self):
        return StringProvider(self.content)

    def get_hints(self):
        return {Hint.FRONT_ARTICLE: True}


def make(path, title, articles):
    with Creator(path).config_indexing(True, "eng") as c:
        for n in range(articles):
            page = f"page-{n}"
            c.add_item(Page(page, f"{title} {n}", f"<html><body><h1>{title} {n}</h1></body></html>"))
        c.set_mainpath("page-0")
        for key, value in {
            "Name": path.stem,
            "Title": title,
            "Description": f"{title} fixture",
            "Language": "eng",
            "Creator": "kiwix-operator",
            "Publisher": "kiwix-operator",
            "Date": "2026-11-24",
        }.items():
            c.add_metadata(key, value)


here = pathlib.Path(__file__).resolve().parent
valid = [("small-a", "Small A", 3), ("small-b", "Small B", 30), ("tiny", "Tiny", 1)]
valid += [(f"book-{n:02d}", f"Book {n:02d}", 1) for n in range(1, 17)]

for stem, title, articles in valid:
    make(here / f"{stem}.zim", title, articles)

small = (here / "small-a.zim").read_bytes()
(here / "truncated.zim").write_bytes(small[: len(small) // 2])

entries = []
for stem, _, _ in valid:
    archive = Archive(str(here / f"{stem}.zim"))
    entries.append({"file": f"{stem}.zim", "uuid": str(archive.uuid), "articleCount": archive.article_count})
(here / "fixtures.json").write_text(json.dumps(entries, indent=2) + "\n")

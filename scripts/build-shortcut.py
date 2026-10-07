#!/usr/bin/env python3
"""Сборка iOS-шортката vdl в подписанный .shortcut.

Шаблон акций — scripts/shortcut-actions.json (реальная сериализация из
Shortcuts.app; формат современный: ключи воркфлоу в корне plist, БЕЗ обёртки
WFWorkflow — завернутый старый формат импортируется пустым). Подпись —
штатный `shortcuts sign` (только macOS).

Использование:
  python3 scripts/build-shortcut.py <база> [выходной_файл]
  база = полный префикс запроса, например:
    http://10.0.1.144:8360/list?token=devtoken&url=
    https://vdl.0x3654.com/list?token=<ТОКЕН>&url=

Выход: неподписанный plist + подписанный .shortcut (modes: anyone).
Прод-артефакт содержит токен: в репо не коммитить, класть на сервер
(/server/vdl/data/shortcut.shortcut, 0600) — keeper отдаёт его на
GET /shortcut?token=…
"""
import json
import plistlib
import subprocess
import sys
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def build(base_url: str, out_signed: str) -> None:
    tpl = json.load(open(os.path.join(HERE, "shortcut-actions.json"), encoding="utf-8"))
    actions = tpl["actions"]

    def subst(o):
        if isinstance(o, dict):
            return {k: subst(v) for k, v in o.items()}
        if isinstance(o, list):
            return [subst(v) for v in o]
        if isinstance(o, str):
            return o.replace("__BASE__", base_url)
        return o

    wf = {
        "WFWorkflowActions": subst(actions),
        "WFWorkflowTypes": ["ActionExtension"],
        "WFWorkflowInputContentItemClasses": ["WFURLContentItem"],
        "WFWorkflowOutputContentItemClasses": [],
        "WFWorkflowClientRelease": "18.0",
        "WFWorkflowClientVersion": "1302.1.3",
        "WFWorkflowMinimumClientVersion": 900,
        "WFWorkflowMinimumClientVersionString": "900",
        "WFWorkflowImportQuestions": [],
        "WFWorkflowHasShortcutInputVariables": True,
        "WFWorkflowHasOutputFallback": False,
        "WFWorkflowNoInputBehavior": {
            "Name": "WFWorkflowNoInputBehaviorStopAndRespond",
            "Parameters": {"Response": ""},
        },
        "WFWorkflowIcon": {"WFWorkflowIconGlyphNumber": 59511,
                           "WFWorkflowIconStartColor": 431817727},
    }
    unsigned = out_signed.replace(".shortcut", "-unsigned.shortcut")
    with open(unsigned, "wb") as f:
        plistlib.dump(wf, f, fmt=plistlib.FMT_BINARY)
    subprocess.run(["shortcuts", "sign", "--mode", "anyone",
                    "--input", unsigned, "--output", out_signed], check=True)
    os.unlink(unsigned)
    print(f"готово: {out_signed} (подписан, режим anyone)")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    build(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "dist/vdl.shortcut")

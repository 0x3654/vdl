#!/usr/bin/env python3
"""Сборка iOS-шортката vdl в подписанный .shortcut.

Шаблон акций — scripts/shortcut-actions.json (реальная сериализация из
Shortcuts.app; формат современный: ключи воркфлоу в корне plist, БЕЗ обёртки
WFWorkflow — завернутый старый формат импортируется пустым). Подпись —
штатный `shortcuts sign` (только macOS).

ГРАБЛЯ (починено): attachmentsByRange хранит ПОЗИЦИЮ переменной в строке;
шаблон снят со старого адреса ({47,1}). После подстановки длинного URL
переменная уезжает — iOS не находил attachment и молча ронял подстановку
(url= приходил пустым, 400). Теперь позиция пересчитывается и проверяется.

Использование:
  python3 scripts/build-shortcut.py <база> [выходной_файл]
  база = полный префикс запроса, например:
    https://vdl.0x3654.com/list?token=<ТОКЕН>&url=
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
OBJ = "￼"  # object replacement char — место переменной в строке


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

    fixed = []
    for a in subst(actions):
        wu = a.get("WFWorkflowActionParameters", {}).get("WFURL")
        if isinstance(wu, dict):
            val = wu.get("Value")
            if isinstance(val, dict) and "attachmentsByRange" in val:
                s = val["string"]
                assert s.count(OBJ) == 1, "ожидается ровно одна переменная в URL"
                pos = s.index(OBJ)
                (old_range, v), = val["attachmentsByRange"].items()
                val["attachmentsByRange"] = {"{%d, 1}" % pos: v}
        fixed.append(a)
    actions = fixed

    wf = {
        "WFWorkflowActions": actions,
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

    # самопроверка: диапазон обязан указывать на объект-символ переменной
    chk = plistlib.load(open(unsigned, "rb"))
    wu = chk["WFWorkflowActions"][0]["WFWorkflowActionParameters"]["WFURL"]["Value"]
    (rng, _), = wu["attachmentsByRange"].items()
    p = int(rng.strip("{}").split(",")[0])
    assert wu["string"][p] == OBJ, "диапазон не указывает на переменную"

    subprocess.run(["shortcuts", "sign", "--mode", "anyone",
                    "--input", unsigned, "--output", out_signed], check=True)
    os.unlink(unsigned)
    print(f"готово: {out_signed} (подписан; переменная на позиции {p})")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    build(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "dist/vdl.shortcut")

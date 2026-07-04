from agent_harness.compaction import CompactionMessage, compact_messages


def test_write_before_compaction_extracts_facts_before_summary_event() -> None:
    messages = [
        CompactionMessage(
            role="user",
            content="我决定使用白色字幕。以后都要快节奏。",
            case_id="case_1",
        ),
        CompactionMessage(role="assistant", content="已记录。", case_id="case_1"),
        CompactionMessage(role="user", content="最新一句保留在窗口里", case_id="case_1"),
    ]

    result = compact_messages(messages, budget=20, counter=len)

    assert "我决定使用白色字幕" in result.extracted_facts
    assert "以后都要快节奏" in result.extracted_facts
    assert result.kept_messages[-1].content == "最新一句保留在窗口里"
    assert result.summary_text.startswith("Earlier conversation summary")
    assert [event.event for event in result.events] == ["BriefUpdated", "ContextCompacted"]
    assert result.events[0].payload["confirmed_facts_append"] == list(result.extracted_facts)

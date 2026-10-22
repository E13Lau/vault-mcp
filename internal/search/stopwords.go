package search

// stopWords are dropped from queries. They are function words, question
// scaffolding ("what", "which", "is there", "related notes") and common English
// stopwords: terms so frequent that they would drown the content words of a
// natural-language question. Documents are never filtered, only queries.
var stopWords = func() map[string]bool {
	words := []string{
		// Chinese function words and particles.
		"的", "了", "是", "在", "和", "与", "及", "或", "有", "我", "你", "他", "她", "它",
		"我们", "你们", "他们", "这", "那", "这个", "那个", "这些", "那些", "就", "也", "都",
		"而", "并", "但", "却", "还", "又", "很", "太", "更", "最", "会", "能", "要", "想",
		"让", "把", "被", "从", "到", "对", "为", "以", "之", "其", "该", "等", "于", "中",
		"上", "下", "里", "内", "外", "前", "后", "时", "个", "些", "种", "啊", "吗", "呢",
		"吧", "呀", "嘛", "哦", "嗯", "哈", "着", "过", "得", "地", "么", "咱", "您",
		// Chinese question words.
		"什么", "怎么", "怎样", "怎么样", "如何", "为什么", "为何", "是否", "能否", "可否",
		"哪里", "哪儿", "哪个", "哪些", "多少", "请问", "一下", "一些", "一点", "什么样",
		"有没有", "有无", "哪些", "几",
		// Question scaffolding that says nothing about the topic.
		"相关", "有关", "笔记", "页面", "内容", "记录", "库里", "库内", "资料", "信息",
		"应该", "需要", "可以", "可能", "一般", "通常", "主要", "目前", "现在",
		"之后", "之前", "以及", "或者", "还是", "因为", "所以", "如果", "虽然", "但是",
		"而且", "然后", "关于", "对于", "通过", "根据", "按照", "由于", "以便", "用来",
		// English stopwords.
		"a", "an", "and", "are", "as", "at", "be", "been", "but", "by", "can", "could",
		"did", "do", "does", "for", "from", "had", "has", "have", "he", "her", "here",
		"him", "his", "how", "i", "if", "in", "into", "is", "it", "its", "me", "my",
		"no", "not", "of", "on", "or", "our", "she", "should", "so", "some", "such",
		"than", "that", "the", "their", "them", "then", "there", "these", "they",
		"this", "those", "to", "was", "we", "were", "what", "when", "where", "which",
		"who", "why", "will", "with", "would", "you", "your",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

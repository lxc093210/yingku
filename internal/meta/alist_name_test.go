package meta

import "testing"

func TestParseAlistFolderName(t *testing.T) {
	cases := []struct {
		name      string
		wantTitle string
		wantYear  int
	}{
		// Real folder names from the user's /电影/4K REMUX catalog.
		{"12勇士 4K REMUX（2018）", "12勇士", 2018},
		{"99号牢房内的博弈 4K REMUX（2017）", "99号牢房内的博弈", 2017},
		{"1917 4K REMUX（2019）", "1917", 2019},
		{"2012 4K REMUX（2009）", "2012", 2009},
		{"2012", "2012", 0},
		{"13刺客", "13刺客", 0},
		{"BJ单身日记 4K REMUX（2001）", "BJ单身日记", 2001},
		// More shapes seen in the same catalog.
		{"760号犯人 4K REMUX（2021）", "760号犯人", 2021},
		{"2001太空漫游 4K REMUX", "2001太空漫游", 0},
		{"信条 4K REMUX 杜比视界（2020）", "信条", 2020},
		{"疯狂的麦克斯4：狂暴之路 2160P（2015）", "疯狂的麦克斯4：狂暴之路", 2015},
	}
	for _, c := range cases {
		title, year := ParseAlistFolderName(c.name)
		if title != c.wantTitle || year != c.wantYear {
			t.Errorf("ParseAlistFolderName(%q) = (%q, %d), want (%q, %d)",
				c.name, title, year, c.wantTitle, c.wantYear)
		}
	}
}

func TestIsAlistCollectionFolder(t *testing.T) {
	if !IsAlistCollectionFolder("007系列 27部") {
		t.Error("007系列 27部 should be a collection")
	}
	if !IsAlistCollectionFolder("漫威合集") {
		t.Error("漫威合集 should be a collection")
	}
	// "N部" multi-film packs are collections too.
	for _, pack := range []string{"哈利波特 8部", "叶问4部", "变形金刚 6部", "复仇者联盟 4部"} {
		if !IsAlistCollectionFolder(pack) {
			t.Errorf("%s should be a collection (N部 pack)", pack)
		}
	}
	if IsAlistCollectionFolder("12勇士 4K REMUX（2018）") {
		t.Error("single title misclassified as collection")
	}
	// 部 inside a title (no leading digit run at the end) is fine.
	if IsAlistCollectionFolder("第一部 4K REMUX（2020）") {
		t.Error("第一部 misclassified as collection")
	}
	// 第N部 is a single film (episode N), not a pack.
	if IsAlistCollectionFolder("星球大战 第9部") {
		t.Error("第9部 misclassified as collection")
	}
}

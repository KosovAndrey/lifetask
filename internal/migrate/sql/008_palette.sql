-- Палитра сфер: чередование тёмных и светлых тонов, проверена на различимость
-- (в т.ч. при дальтонизме) на светлом и тёмном фоне. color — для светлой темы,
-- style.color_dark — для тёмной. Тёплые сферы (продукт, быт, досуг) отделены
-- от холодных (работа, карьера, учёба), здоровье — зелёное.
UPDATE spheres s SET color = v.light, style = COALESCE(s.style, '{}') || jsonb_build_object('color_dark', v.dark)
FROM (VALUES
    ('work',    '#5451A4', '#57579D'),
    ('career',  '#00C4C4', '#12A7A7'),
    ('study',   '#006899', '#006A94'),
    ('product', '#993C23', '#944632'),
    ('home',    '#E79551', '#C4804A'),
    ('leisure', '#7C5700', '#7A5B00'),
    ('health',  '#73C076', '#65A467')
) AS v(slug, light, dark)
WHERE s.slug = v.slug;

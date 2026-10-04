-- «Финансы» сливаются в «Быт»: задач про деньги мало, отдельная сфера только дробит список.
-- Сферу не удаляем, а архивируем — история и ссылки на неё сохраняются.
UPDATE items SET sphere_id = (SELECT id FROM spheres WHERE slug = 'home')
    WHERE sphere_id = (SELECT id FROM spheres WHERE slug = 'finance');
UPDATE projects SET sphere_id = (SELECT id FROM spheres WHERE slug = 'home')
    WHERE sphere_id = (SELECT id FROM spheres WHERE slug = 'finance');
UPDATE spheres SET archived = true WHERE slug = 'finance';
